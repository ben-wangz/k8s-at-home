package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

type inventoryCheckpoint struct {
	SchemaVersion     int                     `json:"schemaVersion"`
	Fence             uint64                  `json:"fence"`
	Scan              inventoryScan           `json:"scan"`
	Build             *inventory.Snapshot     `json:"build"`
	BuildPhase        int                     `json:"buildPhase"`
	BuildToken        string                  `json:"buildToken"`
	BuildPage         *inventory.Cursor       `json:"buildPage,omitempty"`
	ImageBuild        *inventoryImageProgress `json:"imageBuild,omitempty"`
	ProgressAt        time.Time               `json:"progressAt"`
	Steps             uint64                  `json:"steps"`
	Batch             inventoryBatchProgress  `json:"batch"`
	NextScanAt        time.Time               `json:"nextScanAt"`
	NextMaintenanceAt time.Time               `json:"nextMaintenanceAt"`
}

func (s *Service) inventoryConfig() config.Inventory {
	if s.cfg.Inventory == (config.Inventory{}) {
		return config.DefaultInventory()
	}
	return s.cfg.Inventory
}

func (s *Service) initializeInventory(ctx context.Context) {
	var installed time.Time
	err := inventory.Update(ctx, s.store, inventory.Prefix+"installed.json", func(value *time.Time) error {
		if value.IsZero() {
			*value = time.Now().UTC()
		}
		installed = *value
		return nil
	})
	if err != nil {
		s.inventoryFailure(err)
		return
	}
	s.inventoryInstalledAt = installed
}

func verifyInventoryManifest(digest string, data []byte) error {
	return artifact.VerifyDigest(digest, data)
}

func (s *Service) runInventory(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		leaderCtx, leader, release, err := acquireRenewedLease(ctx, s.tasks.Leases(), "inventory/builder", requestID(), time.Minute)
		if err == nil {
			err = s.runInventoryLeader(leaderCtx, leader)
			_ = release()
		}
		if err != nil && !errors.Is(err, lease.ErrHeld) && ctx.Err() == nil {
			s.inventoryFailure(err)
			s.inventoryErrorStatus(ctx, err, delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

func (s *Service) runInventoryLeader(ctx context.Context, leader lease.Lease) error {
	checkpoint, _, err := inventory.Read[inventoryCheckpoint](ctx, s.store, inventory.Prefix+"checkpoint.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := s.claimInventory(ctx, leader, &checkpoint); err != nil {
		return err
	}
	current, _, currentErr := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
	if errors.Is(currentErr, storage.ErrNotFound) || currentErr == nil && !current.ExpiresAt.After(time.Now()) {
		started := time.Now().UTC()
		if checkpoint.Build != nil {
			started = checkpoint.Build.BuildStartedAt.Add(-time.Nanosecond)
		}
		writer := s.inventory.NewWriter(started, s.inventoryConfig().SnapshotRetention, leader.FencingToken)
		for done := false; !done; {
			var err error
			done, err = writer.Assemble(ctx, s.inventoryConfig().BatchSize)
			if err != nil {
				return err
			}
		}
		if err := writer.Publish(ctx, false, []string{"index_backfilling", "untagged_history_not_enumerable"}); err != nil {
			return err
		}
	} else if currentErr != nil {
		return currentErr
	}
	for ctx.Err() == nil {
		if s.inventoryIdle(&checkpoint) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		batchCtx, cancel := context.WithTimeout(ctx, s.inventoryConfig().BatchTimeout)
		started := time.Now()
		err = s.runInventoryBatch(batchCtx, leader, &checkpoint)
		cancel()
		if s.metrics != nil && s.metrics.inventory != nil {
			s.metrics.inventory.batch.Observe(time.Since(started).Seconds())
		}
		if err != nil {
			return err
		}
		s.maintainInventory(ctx, &checkpoint)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return ctx.Err()
}

func (s *Service) advanceInventory(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint) error {
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	if state.Build != nil {
		if err := s.buildInventoryPage(ctx, state); err != nil {
			if errors.Is(err, storage.ErrConditionFailed) {
				state.Build = nil
				state.BuildPhase = 0
				state.BuildToken = ""
				state.BuildPage, state.ImageBuild = nil, nil
				state.Scan.Complete = false
				state.NextScanAt = time.Time{}
			} else {
				return err
			}
		}
	} else if !s.inventoryIdle(state) {
		s.inventoryDirty.Store(false)
		if err := s.scanInventoryPage(ctx, &state.Scan); err != nil {
			return err
		}
		if state.Scan.Phase == 0 && state.Scan.Complete {
			writer := s.inventory.NewWriter(time.Now().UTC(), s.inventoryConfig().SnapshotRetention, leader.FencingToken)
			state.Build = &writer.Snapshot
			gate, err := s.inventoryEpoch(ctx)
			if err != nil {
				return err
			}
			state.Build.GCEpoch = gate.Epoch
			state.BuildPhase = 0
			state.BuildToken = ""
			state.BuildPage, state.ImageBuild = nil, nil
			state.NextScanAt = time.Now().Add(s.inventoryConfig().RefreshInterval)
		}
	}
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	state.ProgressAt = time.Now().UTC()
	state.Steps++
	if budget, ok := ctx.Value(inventoryBudgetKey{}).(*inventoryBudget); ok {
		state.Batch.Steps++
		state.Batch.StorageOperations = budget.operations.Load()
	}
	err := inventory.Update(ctx, s.store, inventory.Prefix+"checkpoint.json", func(old *inventoryCheckpoint) error {
		if old.Fence > state.Fence {
			return inventory.ErrSuperseded
		}
		*old = *state
		return nil
	})
	if err == nil && s.metrics != nil && s.metrics.inventory != nil {
		s.metrics.inventory.progress.Set(float64(state.ProgressAt.Unix()))
		s.metrics.inventory.steps.Set(float64(state.Steps))
	}
	return err
}
