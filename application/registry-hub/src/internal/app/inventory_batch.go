package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) claimInventory(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint) error {
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	// Fence the publication pointer before adopting an older build's frozen pages.
	if err := s.fenceInventoryPublication(ctx, leader.FencingToken); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, inventory.Prefix+"checkpoint.json", func(current *inventoryCheckpoint) error {
		if current.SchemaVersion < 0 || current.SchemaVersion > 2 {
			return storage.ErrCorruptObject
		}
		if current.Fence > leader.FencingToken {
			return inventory.ErrSuperseded
		}
		current.SchemaVersion, current.Fence = 2, leader.FencingToken
		if current.Build != nil {
			current.Build.BuilderFence = leader.FencingToken
		}
		*state = *current
		return nil
	})
}

func (s *Service) fenceInventoryPublication(ctx context.Context, fence uint64) error {
	if _, err := s.store.Head(ctx, inventory.Prefix+"current.json"); errors.Is(err, storage.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, inventory.Prefix+"current.json", func(current *inventory.Snapshot) error {
		if current.BuilderFence > fence {
			return inventory.ErrSuperseded
		}
		if current.SnapshotID != "" {
			current.BuilderFence = fence
		}
		return nil
	})
}

func (s *Service) runInventoryBatch(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint) error {
	ctx, budget := inventoryBudgetContext(ctx)
	state.Batch = inventoryBatchProgress{StartedAt: time.Now().UTC()}
	reserve := min(5*time.Second, s.inventoryConfig().BatchTimeout/4)
	for steps := 0; steps < s.inventoryConfig().BatchSize; steps++ {
		if s.inventoryIdle(state) {
			return nil
		}
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < reserve {
			if s.metrics != nil {
				s.metrics.recordEvent("inventory_budget_yield")
			}
			return nil
		}
		if budget.operations.Load() >= uint64(max(32, s.inventoryConfig().BatchSize*8)) {
			if s.metrics != nil {
				s.metrics.recordEvent("inventory_budget_yield")
			}
			return nil
		}
		if err := s.advanceInventory(ctx, leader, state); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) inventoryIdle(state *inventoryCheckpoint) bool {
	return state.Build == nil && state.Scan.Phase == 0 && state.Scan.Token == "" && state.Scan.Page == nil && state.Scan.Root == nil && len(state.Scan.Links) == 0 && state.NextScanAt.After(time.Now()) && !s.inventoryDirty.Load()
}

func (s *Service) maintainInventory(ctx context.Context, state *inventoryCheckpoint) {
	if state.NextMaintenanceAt.After(time.Now()) {
		return
	}
	state.NextMaintenanceAt = time.Now().Add(30 * time.Second)
	budget := min(5*time.Second, s.inventoryConfig().BatchTimeout/4)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var err error
	reserve := budget / 4
	snapshotsDone, workDone := false, false
	for step := 0; step < s.inventoryConfig().BatchSize; step++ {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < reserve {
			if s.metrics != nil {
				s.metrics.recordEvent("inventory_maintenance_budget_yield")
			}
			break
		}
		started := time.Now()
		if state.MaintenancePhase == 0 && !snapshotsDone {
			snapshotsDone, err = s.cleanupInventorySnapshots(ctx, "")
		} else if !workDone {
			workDone, err = s.cleanupInventoryWork(ctx)
		}
		state.MaintenancePhase = (state.MaintenancePhase + 1) % 2
		reserve = max(reserve, 2*time.Since(started))
		if err != nil || snapshotsDone && workDone {
			break
		}
	}
	if err != nil && !errors.Is(err, storage.ErrConditionFailed) {
		if s.metrics != nil {
			s.metrics.recordEvent("inventory_maintenance_failed")
		}
		if s.logger != nil {
			s.logger.Warn("inventory maintenance deferred", "error", err)
		}
	}
}
