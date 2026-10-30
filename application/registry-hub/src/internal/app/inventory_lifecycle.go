package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type inventoryExecutionFact struct {
	TaskID                string             `json:"taskID"`
	Kind                  tasks.Kind         `json:"kind"`
	Identity              inventory.Identity `json:"identity"`
	RootDigest            string             `json:"rootDigest"`
	Digest                string             `json:"digest"`
	Generation            uint64             `json:"generation"`
	ContentGeneration     uint64             `json:"contentGeneration"`
	RootContentGeneration uint64             `json:"rootContentGeneration"`
	Demand                *tasks.BlobDemand  `json:"demand,omitempty"`
}

type inventoryExecutionProgress struct {
	Phase  int               `json:"phase"`
	Cursor inventory.Cursor  `json:"cursor"`
	Demand *tasks.BlobDemand `json:"demand,omitempty"`
	Nodes  inventory.Cursor  `json:"nodes"`
}

func (s *Service) inventoryStart(ctx context.Context, task *tasks.Task, digest string) error {
	if s.inventory == nil {
		return nil
	}
	generation, err := s.generation(ctx, task.Authority, task.Repository, digest)
	if err != nil {
		return temporary("execution_state", err)
	}
	rootGeneration := generation.Generation
	if task.RootDigest != "" && task.RootDigest != digest {
		root, err := s.generation(ctx, task.Authority, task.Repository, task.RootDigest)
		if err != nil {
			return temporary("execution_state", err)
		}
		rootGeneration = root.Generation
	}
	fact := inventoryExecutionFact{TaskID: task.ID, Kind: task.Kind, Identity: inventoryIdentity(task.Authority, task.Repository, task.Reference), RootDigest: task.RootDigest, Digest: digest, Generation: task.Generation, ContentGeneration: generation.Generation, RootContentGeneration: rootGeneration}
	key := executionSource(task.ID, digest, generation.Generation)
	job, err := (workqueue.Queue{Store: s.store}).Enqueue(ctx, "execution", key, fact, time.Now().UTC())
	if err != nil {
		return temporary("execution_state", err)
	}
	if err := s.indexExecutionSource(ctx, job, fact); err != nil {
		return temporary("execution_state", err)
	}
	return nil
}

func executionSource(taskID, digest string, generation uint64) string {
	return inventory.ID("execution", taskID, digest, strconv.FormatUint(generation, 10))
}

func (s *Service) projectExecutionStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var fact inventoryExecutionFact
	var p inventoryExecutionProgress
	if json.Unmarshal(job.Payload, &fact) != nil || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &p) != nil {
		return p, false, storage.ErrCorruptObject
	}
	current, err := s.generation(ctx, fact.Identity.Authority, fact.Identity.Repository, fact.Digest)
	if err != nil {
		return p, false, err
	}
	if current.Tombstone || current.Generation != fact.ContentGeneration {
		job.Superseded = true
		return p, true, nil
	}
	if err := s.indexExecutionSource(ctx, *job, fact); err != nil {
		return p, false, err
	}
	if fact.Kind == tasks.BlobKind {
		return s.projectBlobExecutionStep(ctx, job, fact, p)
	}
	err = s.projectExecutionFact(ctx, fact, job.CreatedAt, p.Phase)
	if err == nil {
		p.Phase++
	}
	return p, p.Phase >= 3, err
}

func (s *Service) projectExecutionFact(ctx context.Context, fact inventoryExecutionFact, at time.Time, phase int) error {
	digest, generation := fact.RootDigest, fact.RootContentGeneration
	if phase == 2 {
		digest, generation = fact.Digest, fact.ContentGeneration
	}
	current, err := s.generation(ctx, fact.Identity.Authority, fact.Identity.Repository, digest)
	if err != nil {
		return err
	}
	if current.Tombstone || current.Generation != generation {
		return nil
	}
	if phase == 0 {
		binding, _, err := inventory.Read[inventory.Binding](ctx, s.store, inventory.Prefix+"bindings/"+fact.Identity.ID()+".json")
		if errors.Is(err, storage.ErrNotFound) {
			task, readErr := s.tasks.Get(ctx, fact.TaskID)
			if readErr != nil {
				return readErr
			}
			confirmed := task.ManifestObservedAt
			if confirmed.IsZero() {
				confirmed = task.CreatedAt
			}
			binding, err = s.inventoryBinding(ctx, fact.Identity, fact.RootDigest, "task", fact.Generation, confirmed, false)
		}
		if err != nil {
			return err
		}
		if binding.Digest == fact.RootDigest && binding.Generation == fact.Generation && binding.ContentGeneration == generation {
			return s.inventory.StartAt(ctx, inventory.LifecycleVersion(fact.Identity.ID(), binding.Revision, generation), at)
		}
		return nil
	}
	if err := s.inventory.StartAt(ctx, inventory.LifecycleVersion(inventory.ID("platform", fact.Identity.Authority, fact.Identity.Repository, digest), "", generation), at); err != nil {
		return err
	}
	identity := inventoryIdentity(fact.Identity.Authority, fact.Identity.Repository, digest)
	binding, err := s.inventoryBinding(ctx, identity, digest, "execution", generation, at, false)
	if err != nil {
		return err
	}
	return s.inventory.StartAt(ctx, inventory.LifecycleVersion(identity.ID(), binding.Revision, generation), at)
}

func (s *Service) projectBlobExecutionStep(ctx context.Context, job *workqueue.Job, fact inventoryExecutionFact, p inventoryExecutionProgress) (any, bool, error) {
	if p.Demand == nil {
		if fact.Demand != nil {
			if p.Cursor.Done {
				return p, true, nil
			}
			p.Demand = fact.Demand
		} else {
			entry, ok, err := p.Cursor.Peek(ctx, s.store, "solidification/blobs/owners/"+fact.TaskID+"/", 32)
			if err != nil {
				return p, false, err
			}
			if !ok {
				return p, p.Cursor.Done, nil
			}
			demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, entry.Key)
			if err != nil {
				return p, false, err
			}
			if demand.RootTaskID == "" {
				p.Cursor.Advance()
				return p, false, nil
			}
			p.Demand = &demand
		}
	}
	root, err := s.tasks.Get(ctx, p.Demand.RootTaskID)
	if errors.Is(err, storage.ErrNotFound) {
		if fact.Demand != nil {
			return p, true, nil
		}
		p.Demand = nil
		p.Cursor.Advance()
		return p, false, nil
	}
	if err != nil {
		return p, false, err
	}
	if p.Phase == 0 {
		node, err := s.records.GetGraphNode(ctx, root.ID, tasks.GraphManifest, root.RootDigest)
		if errors.Is(err, storage.ErrNotFound) {
			p.Phase = 1
			return p, false, nil
		}
		if err != nil {
			return p, false, err
		}
		if node.ContentGeneration == nil {
			p.Phase = 1
			return p, false, nil
		}
		rootFact := inventoryExecutionFact{TaskID: root.ID, Identity: inventoryIdentity(root.Authority, root.Repository, root.Reference), RootDigest: root.RootDigest, Digest: root.RootDigest, Generation: root.Generation, ContentGeneration: *node.ContentGeneration, RootContentGeneration: *node.ContentGeneration}
		if err := s.indexBlobExecutionManifest(ctx, *job, fact, root, *p.Demand, node.Digest, *node.ContentGeneration, true); err != nil {
			return p, false, err
		}
		at := job.CreatedAt
		if p.Demand.CreatedAt.After(at) {
			at = p.Demand.CreatedAt
		}
		if err := s.projectExecutionFact(ctx, rootFact, at, 0); err != nil {
			return p, false, err
		}
		if err := s.projectExecutionFact(ctx, rootFact, at, 1); err != nil {
			return p, false, err
		}
		p.Phase = 1
		return p, false, nil
	}
	return s.projectBlobExecutionScope(ctx, job, root, p)
}
