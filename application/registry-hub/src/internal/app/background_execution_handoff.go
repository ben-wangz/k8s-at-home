package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type executionHandoffProgress struct {
	Cursor inventory.Cursor `json:"cursor"`
	Fence  uint64           `json:"fence"`
}

func (s *Service) demandExecutionWork(ctx context.Context, demand tasks.BlobDemand) (workqueue.Job, bool, error) {
	intent, err := s.records.GetBlobIntent(ctx, demand.IntentID)
	if err != nil {
		return workqueue.Job{}, false, err
	}
	q := workqueue.Queue{Store: s.store}
	started, err := q.Read(ctx, "execution", workqueue.ID("execution", executionSource(intent.ID, intent.Identity.Digest, intent.Generation)))
	if errors.Is(err, storage.ErrNotFound) {
		return workqueue.Job{}, false, nil
	}
	if err != nil {
		return workqueue.Job{}, false, err
	}
	var fact inventoryExecutionFact
	if json.Unmarshal(started.Payload, &fact) != nil {
		return workqueue.Job{}, false, storage.ErrCorruptObject
	}
	fact.Demand = &demand
	job, err := q.Enqueue(ctx, "execution", inventory.ID(started.ID, demand.DemandID, demand.CreatedAt.String()), fact, started.CreatedAt)
	if err == nil {
		err = s.indexBlobDemandRoot(ctx, job, fact, demand)
	}
	return job, true, err
}

func (s *Service) handoffExecutionDemandsStep(ctx context.Context, task *tasks.Task) (bool, error) {
	key := inventory.Prefix + "execution-handoffs/" + task.ID + ".json"
	previous, _, err := inventory.Read[executionHandoffProgress](ctx, s.store, key)
	if err == nil && previous.Cursor.Done {
		return true, nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	holder, err := s.tasks.Leases().Acquire(ctx, "inventory/execution-handoff/"+task.ID, requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer s.tasks.Leases().Release(ctx, holder)
	progress, _, err := inventory.Read[executionHandoffProgress](ctx, s.store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if progress.Cursor.Done {
		return true, nil
	}
	prefix := "solidification/blobs/root-owners/" + task.ID + "/"
	if task.Kind == tasks.BlobKind {
		prefix = "solidification/blobs/owners/" + task.ID + "/"
	}
	entry, ok, err := progress.Cursor.Peek(ctx, s.store, prefix, 16)
	if err != nil {
		return false, err
	}
	if ok {
		demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, entry.Key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
		ready := true
		if err == nil && demand.RootTaskID != "" {
			job, exists, err := s.demandExecutionWork(ctx, demand)
			if err != nil {
				return false, err
			}
			if exists && job.State != "done" {
				ready = false
				if err := s.advanceExecutionHandoffJob(ctx, job); err != nil {
					return false, err
				}
			}
		}
		if ready {
			progress.Cursor.Advance()
		}
	}
	if err := s.tasks.Leases().Validate(ctx, holder); err != nil {
		return false, err
	}
	progress.Fence = holder.FencingToken
	err = inventory.Update(ctx, s.store, key, func(current *executionHandoffProgress) error {
		if current.Fence > holder.FencingToken {
			return inventory.ErrSuperseded
		}
		*current = progress
		return nil
	})
	return progress.Cursor.Done && err == nil, err
}

func (s *Service) advanceExecutionHandoffJob(ctx context.Context, job workqueue.Job) error {
	if job.State == "failed" {
		return storage.ErrUnavailable
	}
	q := workqueue.Queue{Store: s.store}
	claimed, err := q.Claim(ctx, job)
	if errors.Is(err, storage.ErrConditionFailed) {
		return storage.ErrUnavailable
	}
	if err != nil {
		return err
	}
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return q.Validate(ctx, claimed) })
	progress, done, cause := s.projectExecutionStep(guarded, &claimed)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(cause, q.Finish(finishCtx, &claimed, progress, done, cause))
}
