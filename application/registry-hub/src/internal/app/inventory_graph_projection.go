package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

type inventoryGraphProjection struct {
	Epoch    uint64                   `json:"epoch"`
	Fence    uint64                   `json:"fence,omitempty"`
	Task     tasks.Task               `json:"task"`
	Version  string                   `json:"version"`
	Prefix   string                   `json:"prefix"`
	Phase    int                      `json:"phase"`
	Cursor   inventory.Cursor         `json:"cursor"`
	Nodes    inventory.Cursor         `json:"nodes"`
	Node     *inventoryNodeProjection `json:"node,omitempty"`
	Restart  bool                     `json:"restart,omitempty"`
	Complete bool                     `json:"complete,omitempty"`
	Restarts uint64                   `json:"restarts,omitempty"`
	Changes  []inventoryChange        `json:"changes,omitempty"`
}

func (s *Service) newInventoryProjection(ctx context.Context, id string) (*inventoryGraphProjection, error) {
	task, version, err := inventory.Read[tasks.Task](ctx, s.store, "tasks/state/"+id+".json")
	if err != nil {
		return nil, err
	}
	if task.ID != id {
		return nil, storage.ErrCorruptObject
	}
	task.Graph, task.Checkpoints = nil, nil
	task.DiagnosticOutbox = nil
	p := &inventoryGraphProjection{Task: task, Version: version, Prefix: inventory.Prefix + "projections/source/" + id + "/" + inventory.ID("source", version, requestID()) + "/"}
	if s.gate != nil {
		p.Epoch = s.gate.State().Epoch
	}
	if err := s.captureInventoryProjectionChanges(ctx, p); err != nil {
		return nil, err
	}
	p.Complete, err = s.reuseInventoryProjection(ctx, p)
	for _, change := range p.Changes {
		if strings.HasPrefix(change.Key, "solidification/graphs/") {
			p.Complete = false
		}
	}
	if err == nil && p.Complete {
		err = s.acknowledgeInventoryProjection(ctx, p)
	}
	return p, err
}

func (s *Service) advanceInventoryProjection(ctx context.Context, p *inventoryGraphProjection) error {
	if p.Complete {
		return nil
	}
	switch p.Phase {
	case 0:
		done, _, err := inventory.Read[bool](ctx, s.store, inventory.Prefix+"handoffs/"+p.Task.ID+".json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if done || p.Task.Kind != tasks.RootKind {
			if err := s.acknowledgeInventoryProjection(ctx, p); err != nil {
				return err
			}
			p.Complete = true
			return nil
		}
		if err := s.preserveInventoryTaskWork(ctx, p.Task); err != nil {
			return err
		}
		p.Phase++
	case 1:
		if !p.Nodes.Done {
			return s.captureInventoryGraphNode(ctx, p)
		}
		return s.indexInventoryEdge(ctx, p)
	case 2:
		return s.projectInventoryGraphNode(ctx, p)
	case 3:
		current, version, err := inventory.Read[tasks.Task](ctx, s.store, "tasks/state/"+p.Task.ID+".json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && version != p.Version {
			p.Restart = p.Restart || current.Generation != p.Task.Generation || current.RootDigest != p.Task.RootDigest || current.GraphVersion != p.Task.GraphVersion || current.Discovery != p.Task.Discovery
			if !p.Restart {
				if err := s.preserveInventoryTaskWork(ctx, current); err != nil {
					return err
				}
				current.Graph, current.Checkpoints = nil, nil
				p.Task, p.Version = current, version
			}
		}
		p.Cursor, p.Phase = inventory.Cursor{}, 4
	case 4:
		entry, ok, err := p.Cursor.Peek(ctx, s.store, p.Prefix, 100)
		if err != nil {
			return err
		}
		if ok {
			if err := s.store.Delete(ctx, entry.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			p.Cursor.Advance()
		}
		if p.Cursor.Done {
			if !p.Restart {
				if err := s.saveInventoryProjectionReceipt(ctx, p); err != nil {
					return err
				}
				if err := s.acknowledgeInventoryProjection(ctx, p); err != nil {
					return err
				}
				p.Complete = true
			}
			if p.Complete && p.Task.CleanupStarted {
				if err := inventory.Update(ctx, s.store, inventory.Prefix+"handoffs/"+p.Task.ID+".json", func(value *bool) error { *value = true; return nil }); err != nil {
					return err
				}
			}
			if p.Restart {
				next, err := s.newInventoryProjection(ctx, p.Task.ID)
				if err != nil {
					return err
				}
				if strings.Contains(p.Prefix, "/projections/handoff/") {
					next.Prefix = strings.Replace(next.Prefix, "/projections/source/", "/projections/handoff/", 1)
				}
				next.Restarts = p.Restarts + 1
				if s.metrics != nil {
					s.metrics.recordEvent("inventory_graph_restarted")
				}
				*p = *next
			}
		}
	default:
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Service) preserveInventoryTaskWork(ctx context.Context, task tasks.Task) error {
	identity := inventoryIdentity(task.Authority, task.Repository, task.Reference)
	confirmed := task.ManifestObservedAt
	if confirmed.IsZero() {
		confirmed = task.CreatedAt
	}
	if _, err := s.inventoryBinding(ctx, identity, task.RootDigest, "task", task.Generation, confirmed, false); err != nil {
		return err
	}
	work := inventoryWork{Identity: identity, Digest: task.RootDigest, Generation: task.Generation, CreatedAt: task.CreatedAt, UpdatedAt: task.UpdatedAt, Work: inventory.Work{ID: task.ID, Kind: string(task.Kind), Status: string(task.Status), WaitReason: task.WaitReason, ErrorClass: task.ErrorClass, ErrorMessage: task.ErrorMessage, NextAttemptAt: optionalTime(task.NextAttemptAt), LastProgressAt: optionalTime(task.LastProgressAt)}}
	return s.saveInventoryWork(ctx, inventory.Prefix+"work/"+identity.ID()+"/"+task.ID+".json", work)
}

func inventoryParentPrefix(p *inventoryGraphProjection, digest string) string {
	return p.Prefix + "parents/" + inventory.ID("parent", digest) + "/"
}

func (s *Service) indexInventoryEdge(ctx context.Context, p *inventoryGraphProjection) error {
	entry, ok, err := p.Cursor.Peek(ctx, s.store, "solidification/graphs/"+p.Task.ID+"/edges/", 100)
	if err != nil {
		return err
	}
	if ok {
		edge, _, err := inventory.Read[tasks.DependencyEdge](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if edge.SchemaVersion != 1 || edge.RootTaskID != p.Task.ID || edge.Order < 0 || edge.ChildDigest != edge.Descriptor.Digest || artifactDigest(edge.ParentDigest) != nil || artifactDigest(edge.ChildDigest) != nil || edge.Descriptor.MediaType == "" || edge.Descriptor.Size < 0 || edge.ChildRole != tasks.GraphBlob && edge.ChildRole != tasks.GraphManifest {
			return storage.ErrCorruptObject
		}
		if edge.ParentRole == tasks.GraphManifest {
			key := inventoryParentPrefix(p, edge.ParentDigest) + fmt.Sprintf("%010d.json", edge.Order)
			if err := s.inventory.Immutable(ctx, key, edge); err != nil {
				return err
			}
		}
		p.Cursor.Advance()
	}
	if p.Cursor.Done {
		p.Cursor, p.Phase = inventory.Cursor{}, 2
	}
	return nil
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
