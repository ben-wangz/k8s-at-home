package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) advanceInventorySources(ctx context.Context, state *inventoryCheckpoint) (bool, error) {
	if s.inventoryReconcile.Swap(false) && !state.NextReconcileAt.IsZero() {
		state.NextReconcileAt = time.Time{}
		return false, nil
	}
	if state.Delta != nil || state.Scan.Complete && state.Scan.Phase == 0 && time.Now().Before(state.NextReconcileAt) {
		if state.Delta == nil {
			state.Delta = &inventoryDeltaProgress{}
		}
		state.Delta.Fence = state.Fence
		if !state.NextReconcileAt.IsZero() {
			for _, change := range state.Delta.Changes {
				if change.Key == "distribution/catalog" {
					// Commit the reconciliation requirement before retiring its notification.
					state.NextReconcileAt = time.Time{}
					return false, nil
				}
			}
		}
		reserve := min(5*time.Second, s.inventoryConfig().BatchTimeout/4)
		for step := 0; step < min(4, s.inventoryConfig().BatchSize); step++ {
			if deadline, ok := ctx.Deadline(); step > 0 && ok && time.Until(deadline) < reserve {
				break
			}
			prefix := ""
			if state.Delta.Source != nil {
				prefix = state.Delta.Source.Prefix
			}
			started := time.Now()
			done, err := s.advanceInventoryDelta(ctx, state.Delta)
			if err != nil {
				return false, err
			}
			if done {
				state.Delta = nil
				if !time.Now().Before(state.NextReconcileAt) {
					state.Scan.Complete = false
					return false, nil
				}
				return true, nil
			}
			if state.Delta.Source != nil && state.Delta.Source.Prefix != prefix {
				break
			}
			for _, change := range state.Delta.Changes {
				if change.Key == "distribution/catalog" && !state.NextReconcileAt.IsZero() {
					state.NextReconcileAt = time.Time{}
					return false, nil
				}
			}
			reserve = max(reserve, 2*time.Since(started))
		}
		return false, nil
	}
	if err := s.scanInventoryUnit(ctx, &state.Scan); err != nil {
		return false, err
	}
	if state.Scan.Phase == 0 && state.Scan.Complete {
		state.NextReconcileAt = time.Now().Add(max(5*time.Minute, s.inventoryConfig().RefreshInterval))
		return true, nil
	}
	return false, nil
}

func (s *Service) inventoryImageDirtyRevision(ctx context.Context, id string) (string, error) {
	value, _, err := inventory.Read[inventoryDirtyEntity](ctx, s.store, inventoryDirtyKey(id))
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	return value.Revision, err
}

func (s *Service) acknowledgeInventoryImage(ctx context.Context, p *inventoryImageProgress) error {
	if p.DirtyRevision == "" {
		return nil
	}
	return inventory.Update(ctx, s.store, inventoryDirtyKey(p.Image.ID), func(value *inventoryDirtyEntity) error {
		if value.Fence > p.Fence {
			return inventory.ErrSuperseded
		}
		if value.Revision == p.DirtyRevision {
			value.Captured = p.DirtyRevision
			value.Fence = p.Fence
		}
		return nil
	})
}
