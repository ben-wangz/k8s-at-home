package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func (s *solidifier) PreserveTaskHistory(ctx context.Context, task *tasks.Task) error {
	for {
		done, err := s.PreserveTaskHistoryStep(ctx, task)
		if err != nil || done {
			return err
		}
	}
}

// Cleanup may delete a graph only after this method reports a complete handoff.
func (s *solidifier) PreserveTaskHistoryStep(ctx context.Context, task *tasks.Task) (bool, error) {
	if s.service.inventory == nil {
		return true, nil
	}
	if done, err := s.service.handoffExecutionDemandsStep(ctx, task); err != nil || !done {
		return false, err
	}
	if task.Kind != tasks.RootKind {
		return true, nil
	}
	svc := s.service
	key := inventory.Prefix + "handoffs/" + task.ID + ".json"
	progressKey := inventory.Prefix + "handoff-progress/" + task.ID + ".json"
	done, _, err := inventory.Read[bool](ctx, svc.store, key)
	if err == nil && done {
		_, err := svc.store.Head(ctx, progressKey)
		if errors.Is(err, storage.ErrNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	holder, err := svc.tasks.Leases().Acquire(ctx, "inventory/handoff/"+task.ID, requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer svc.tasks.Leases().Release(ctx, holder)
	p, _, err := inventory.Read[inventoryGraphProjection](ctx, svc.store, progressKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if p.Task.ID == "" {
		next, err := svc.newInventoryProjection(ctx, task.ID)
		if err != nil {
			return false, err
		}
		p = *next
		p.Prefix = strings.Replace(p.Prefix, "/projections/source/", "/projections/handoff/", 1)
	}
	if done {
		if p.Phase != 4 {
			p.Phase, p.Cursor = 4, inventory.Cursor{}
		}
		p.Restart = false
	}
	if err := svc.advanceInventoryProjection(ctx, &p); err != nil {
		return false, err
	}
	if err := svc.tasks.Leases().Validate(ctx, holder); err != nil {
		return false, err
	}
	p.Fence = holder.FencingToken
	err = inventory.Update(ctx, svc.store, progressKey, func(value *inventoryGraphProjection) error {
		if value.Fence > holder.FencingToken {
			return inventory.ErrSuperseded
		}
		*value = p
		return nil
	})
	if err != nil {
		return false, err
	}
	if p.Complete {
		err := inventory.Update(ctx, svc.store, key, func(value *bool) error { *value = true; return nil })
		if err != nil {
			return false, err
		}
		err = svc.store.Delete(ctx, progressKey)
		if errors.Is(err, storage.ErrNotFound) {
			err = nil
		}
		return err == nil, err
	}
	return false, nil
}
