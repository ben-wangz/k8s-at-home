package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) PreserveTaskHistory(ctx context.Context, task *tasks.Task) error {
	if s.service.inventory == nil {
		return nil
	}
	key := inventory.Prefix + "handoffs/" + task.ID + ".json"
	done, _, err := inventory.Read[bool](ctx, s.service.store, key)
	if err == nil && done {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := s.service.preserveInventoryTask(ctx, *task); err != nil {
		return err
	}
	return inventory.Update(ctx, s.service.store, key, func(value *bool) error { *value = true; return nil })
}

func (s *Service) preserveInventoryTask(ctx context.Context, task tasks.Task) error {
	if task.Kind != tasks.RootKind {
		return nil
	}
	done, _, err := inventory.Read[bool](ctx, s.store, inventory.Prefix+"handoffs/"+task.ID+".json")
	if err == nil && done {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	identity := inventoryIdentity(task.Authority, task.Repository, task.Reference)
	_, err = s.inventoryBinding(ctx, identity, task.RootDigest, "task", task.Generation, task.CreatedAt, false)
	if err != nil {
		return err
	}
	key := inventory.Prefix + "work/" + identity.ID() + "/" + task.ID + ".json"
	work := inventoryWork{Identity: identity, Digest: task.RootDigest, Generation: task.Generation, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, Work: inventory.Work{ID: task.ID, Kind: string(task.Kind), Status: string(task.Status), WaitReason: task.WaitReason, ErrorClass: task.ErrorClass, ErrorMessage: task.ErrorMessage, NextAttemptAt: optionalTime(task.NextAttemptAt), LastProgressAt: optionalTime(task.LastProgressAt)}}
	if err := s.saveInventoryWork(ctx, key, work); err != nil {
		return err
	}
	return s.preserveInventoryGraph(ctx, task)
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Service) preserveInventoryGraph(ctx context.Context, task tasks.Task) error {
	token := ""
	for {
		nodes, next, err := s.records.ListGraphNodes(ctx, task.ID, token, 100)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if err := s.preserveInventoryNode(ctx, task, node); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (s *Service) preserveInventoryNode(ctx context.Context, task tasks.Task, node tasks.GraphNode) error {
	identity := inventoryIdentity(task.Authority, task.Repository, "")
	if node.Role == tasks.GraphManifest {
		generation := uint64(0)
		if node.ContentGeneration != nil {
			generation = *node.ContentGeneration
		}
		content := inventory.Content{Identity: identity, ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: node.Digest, MediaType: node.MediaType, Size: node.ActualSize}, Generation: generation, ClosureKnown: node.DiscoveryComplete, Children: []inventory.ObjectRef{}, UpdatedAt: task.UpdatedAt}
		content.Source = "graph"
		token := ""
		for {
			edges, next, err := s.records.ListDependencyEdges(ctx, task.ID, token, 100)
			if err != nil {
				return err
			}
			for _, edge := range edges {
				if edge.ParentRole != tasks.GraphManifest || edge.ParentDigest != node.Digest {
					continue
				}
				role := "layer"
				if edge.ChildRole == tasks.GraphManifest {
					role = "manifest"
				}
				descriptor := artifact.Descriptor{Digest: edge.ChildDigest, MediaType: edge.Descriptor.MediaType, Size: edge.Descriptor.Size, Platform: edge.Descriptor.Platform, ArtifactType: edge.Descriptor.ArtifactType}
				content.Children = append(content.Children, inventoryDescriptor(descriptor, role))
			}
			if next == "" {
				break
			}
			token = next
		}
		if err := s.inventory.SaveContent(ctx, content); err != nil {
			return err
		}
		if err := s.linkInventoryContent(ctx, content); err != nil {
			return err
		}
		if err := s.inventoryChildDescriptors(ctx, content); err != nil {
			return err
		}
		work, _, err := inventory.Read[inventoryWork](ctx, s.store, inventory.Prefix+"work/"+inventoryIdentity(task.Authority, task.Repository, task.Reference).ID()+"/"+task.ID+".json")
		if err != nil {
			return err
		}
		work.ContentGeneration = generation
		if err := s.saveInventoryWork(ctx, inventory.Prefix+"content-work/"+identity.ContentID(node.Digest)+"/"+task.ID+".json", work); err != nil {
			return err
		}
	}
	if node.ContentGeneration == nil || node.LocalState != "anchored" {
		return nil
	}
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
	proof := inventoryProof{SchemaVersion: 1, Generation: *node.ContentGeneration, Epoch: node.GCEpoch, State: node.LocalState, ObservedAt: observation.ObservedAt}
	return inventory.Update(ctx, s.store, inventory.Prefix+"proofs/"+identity.ContentID(node.Digest)+".json", func(old *inventoryProof) error {
		if old.Generation < proof.Generation || old.Generation == proof.Generation && old.Epoch <= proof.Epoch {
			*old = proof
		}
		return nil
	})
}
