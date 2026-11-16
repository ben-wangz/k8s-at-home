package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

type inventoryLeaderContextKey struct{}

func (s *Service) advanceInventorySources(ctx context.Context, state *inventoryCheckpoint) (bool, error) {
	fullReconcile := !state.Scan.Complete || state.NextReconcileAt.IsZero() || !time.Now().Before(state.NextReconcileAt)
	if fullReconcile {
		ctx = withStorageReason(ctx, "inventory_full_reconcile")
	} else {
		ctx = withStorageReason(ctx, "inventory_refresh")
	}
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
			if len(state.Delta.Changes) > 0 {
				state.Delta.Changed = true
			}
			if done {
				changed := state.Delta.Changed
				state.Delta = nil
				if !time.Now().Before(state.NextReconcileAt) {
					state.Scan.Complete = false
					return false, nil
				}
				now := time.Now().UTC()
				state.LastSourceCheckAt = now
				if changed {
					return true, nil
				}
				leader, ok := ctx.Value(inventoryLeaderContextKey{}).(lease.Lease)
				if !ok {
					return false, errors.New("inventory source freshness update requires builder lease")
				}
				current, err := s.refreshInventorySourceCheck(ctx, leader, state)
				if err != nil {
					return false, err
				}
				if current {
					completedAt := time.Now().UTC()
					state.NextScanAt = completedAt.Add(s.inventoryConfig().RefreshInterval)
					state.NextEventScanAt = completedAt.Add(inventoryMinimumIdle)
					if !state.ScanStartedAt.IsZero() {
						state.LastRefreshSeconds = completedAt.Sub(state.ScanStartedAt).Seconds()
						state.ScanStartedAt = time.Time{}
					}
				}
				return !current, nil
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
		state.LastSourceCheckAt = time.Now().UTC()
		state.NextReconcileAt = time.Now().Add(max(5*time.Minute, s.inventoryConfig().RefreshInterval))
		state.FullReconcilePending = true
		return true, nil
	}
	return false, nil
}

func (s *Service) refreshInventorySourceCheck(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint) (bool, error) {
	current, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if current.SnapshotID == "" || !current.ExpiresAt.After(time.Now()) {
		return false, nil
	}
	checkpointCtx := inventory.WithWriteGuard(ctx, func(ctx context.Context) error {
		return s.tasks.Leases().Validate(ctx, leader)
	})
	err = inventory.Update(checkpointCtx, s.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error {
		if value.BuilderFence > state.Fence {
			return inventory.ErrSuperseded
		}
		if value.SnapshotID != current.SnapshotID {
			return storage.ErrConditionFailed
		}
		value.LastSourceCheckAt = state.LastSourceCheckAt
		value.ExpiresAt = maxTime(value.ExpiresAt, state.LastSourceCheckAt.Add(s.inventoryConfig().SnapshotRetention))
		return nil
	})
	return err == nil, err
}

func maxTime(first, second time.Time) time.Time {
	if first.After(second) {
		return first
	}
	return second
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
