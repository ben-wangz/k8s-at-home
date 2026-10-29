package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
)

type inventoryNodeProjection struct {
	Key     string            `json:"key"`
	Version string            `json:"version"`
	Node    tasks.GraphNode   `json:"node"`
	Content inventory.Content `json:"content"`
	Cursor  inventory.Cursor  `json:"cursor"`
	Phase   int               `json:"phase"`
}

type inventoryCapturedNode struct {
	Key     string          `json:"key"`
	Version string          `json:"version"`
	Node    tasks.GraphNode `json:"node"`
}

func (s *Service) captureInventoryGraphNode(ctx context.Context, p *inventoryGraphProjection) error {
	entry, ok, err := p.Nodes.Peek(ctx, s.store, "solidification/graphs/"+p.Task.ID+"/nodes/", 100)
	if err != nil {
		return err
	}
	if ok {
		key := p.Prefix + "nodes/" + entry.Key[strings.LastIndex(entry.Key, "/")+1:]
		captured, _, err := inventory.Read[inventoryCapturedNode](ctx, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			node, version, err := inventory.Read[tasks.GraphNode](ctx, s.store, entry.Key)
			if err != nil {
				return err
			}
			if err := s.inventory.Immutable(ctx, key, inventoryCapturedNode{Key: entry.Key, Version: version, Node: node}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if captured.Key != entry.Key || captured.Node.RootTaskID != p.Task.ID || captured.Version == "" {
			return storage.ErrCorruptObject
		}
		p.Nodes.Advance()
	}
	return nil
}

func (s *Service) projectInventoryGraphNode(ctx context.Context, p *inventoryGraphProjection) error {
	if p.Node != nil {
		done, err := s.advanceInventoryNode(ctx, p)
		if err == nil && done {
			p.Node = nil
			p.Cursor.Advance()
		}
		return err
	}
	entry, ok, err := p.Cursor.Peek(ctx, s.store, p.Prefix+"nodes/", 100)
	if err != nil {
		return err
	}
	if !ok {
		if p.Cursor.Done {
			p.Phase++
		}
		return nil
	}
	captured, _, err := inventory.Read[inventoryCapturedNode](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	node, version := captured.Node, captured.Version
	if node.SchemaVersion != 1 || node.RootTaskID != p.Task.ID || artifactDigest(node.Digest) != nil || node.Role != tasks.GraphManifest && node.Role != tasks.GraphBlob || node.MediaType == "" || node.ActualSize < 0 || node.DeclaredSize < 0 {
		return storage.ErrCorruptObject
	}
	generation := uint64(0)
	if node.ContentGeneration != nil {
		generation = *node.ContentGeneration
	}
	content := inventory.Content{Identity: inventoryIdentity(p.Task.Authority, p.Task.Repository, ""), ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: node.Digest, MediaType: node.MediaType, Size: node.ActualSize}, Generation: generation, Source: "graph", Children: []inventory.ObjectRef{}, UpdatedAt: p.Task.UpdatedAt}
	p.Node = &inventoryNodeProjection{Key: captured.Key, Version: version, Node: node, Content: content}
	if node.Role != tasks.GraphManifest {
		p.Node.Phase = 3
	}
	return nil
}

func (s *Service) advanceInventoryNode(ctx context.Context, p *inventoryGraphProjection) (bool, error) {
	n := p.Node
	switch n.Phase {
	case 0:
		return false, s.projectInventoryChild(ctx, p)
	case 1:
		current, version, err := inventory.Read[tasks.GraphNode](ctx, s.store, n.Key)
		if err != nil {
			return false, err
		}
		if version != n.Version {
			if current.DiscoveryComplete != n.Node.DiscoveryComplete || current.Digest != n.Node.Digest || current.MediaType != n.Node.MediaType || current.ActualSize != n.Node.ActualSize {
				p.Node = nil
				p.Restart, p.Phase, p.Cursor = true, 4, inventory.Cursor{}
				return false, nil
			}
			n.Node, n.Version = current, version
			n.Content.Generation = 0
			if current.ContentGeneration != nil {
				n.Content.Generation = *current.ContentGeneration
			}
		}
		n.Content.ClosureKnown = n.Node.DiscoveryComplete
		if err := s.inventory.SaveContent(ctx, n.Content); err != nil {
			return false, err
		}
		n.Phase++
	case 2:
		parent := n.Content
		parent.Children = nil
		if err := s.linkInventoryContent(ctx, parent); err != nil {
			return false, err
		}
		work, _, err := inventory.Read[inventoryWork](ctx, s.store, inventory.Prefix+"work/"+inventoryIdentity(p.Task.Authority, p.Task.Repository, p.Task.Reference).ID()+"/"+p.Task.ID+".json")
		if err != nil {
			return false, err
		}
		work.ContentGeneration = n.Content.Generation
		if err := s.saveInventoryWork(ctx, inventory.Prefix+"content-work/"+n.Content.ContentID(n.Node.Digest)+"/"+p.Task.ID+".json", work); err != nil {
			return false, err
		}
		n.Phase++
	case 3:
		return true, s.preserveInventoryProof(ctx, p.Task, n.Node)
	default:
		return false, storage.ErrCorruptObject
	}
	return false, nil
}

func (s *Service) projectInventoryChild(ctx context.Context, p *inventoryGraphProjection) error {
	n := p.Node
	entry, ok, err := n.Cursor.Peek(ctx, s.store, inventoryParentPrefix(p, n.Node.Digest), 100)
	if err != nil {
		return err
	}
	if ok {
		edge, _, err := inventory.Read[tasks.DependencyEdge](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if len(n.Content.Children) >= s.cfg.Limits.MaxDescriptors {
			return storage.ErrCorruptObject
		}
		role := "layer"
		if edge.ChildRole == tasks.GraphManifest {
			role = "manifest"
		}
		descriptor := artifact.Descriptor{Digest: edge.ChildDigest, MediaType: edge.Descriptor.MediaType, Size: edge.Descriptor.Size, Platform: edge.Descriptor.Platform, ArtifactType: edge.Descriptor.ArtifactType}
		child := inventoryDescriptor(descriptor, role)
		parent := n.Content
		parent.Children = []inventory.ObjectRef{child}
		if err := s.linkInventoryContent(ctx, parent); err != nil {
			return err
		}
		if err := s.inventoryChildDescriptors(ctx, parent); err != nil {
			return err
		}
		n.Content.Children = append(n.Content.Children, child)
		n.Cursor.Advance()
	}
	if n.Cursor.Done {
		n.Phase++
	}
	return nil
}

func (s *Service) preserveInventoryProof(ctx context.Context, task tasks.Task, node tasks.GraphNode) error {
	if node.ContentGeneration == nil || node.LocalState != "anchored" {
		return nil
	}
	identity := inventoryIdentity(task.Authority, task.Repository, "")
	scopeID, err := protectionScopeID(task.Authority, task.Repository, node.Digest, task.Generation)
	if err != nil {
		return err
	}
	if node.Role == tasks.GraphManifest {
		session, err := s.gate.readProtectionSession(ctx, stagedSessionID(scopeID))
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if session.State != "anchored" && (session.State != "released" || session.TerminalReason != "anchored") {
			return nil
		}
	}
	observation, err := s.records.FindLocalObservation(ctx, task.Authority, task.Repository, node.Role, node.Digest, node.ActualSize, *node.ContentGeneration, node.GCEpoch)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.saveInventoryProof(ctx, identity.ContentID(node.Digest), inventoryProof{SchemaVersion: 1, Generation: *node.ContentGeneration, Epoch: node.GCEpoch, State: node.LocalState, ObservedAt: observation.ObservedAt})
}
