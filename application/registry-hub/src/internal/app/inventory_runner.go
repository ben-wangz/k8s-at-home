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
	SchemaVersion      int                     `json:"schemaVersion"`
	Fence              uint64                  `json:"fence"`
	Scan               inventoryScan           `json:"scan"`
	Build              *inventory.Snapshot     `json:"build"`
	BuildPhase         int                     `json:"buildPhase"`
	BuildToken         string                  `json:"buildToken"`
	BuildPage          *inventory.Cursor       `json:"buildPage,omitempty"`
	ImageBuild         *inventoryImageProgress `json:"imageBuild,omitempty"`
	ProgressAt         time.Time               `json:"progressAt"`
	Steps              uint64                  `json:"steps"`
	ScanStartedAt      time.Time               `json:"scanStartedAt,omitempty"`
	LastPublishedAt    time.Time               `json:"lastPublishedAt,omitempty"`
	LastCompleteAt     time.Time               `json:"lastCompleteAt,omitempty"`
	LastBuildSeconds   float64                 `json:"lastBuildSeconds"`
	LastRefreshSeconds float64                 `json:"lastRefreshSeconds"`
	Batch              inventoryBatchProgress  `json:"batch"`
	NextScanAt         time.Time               `json:"nextScanAt"`
	NextMaintenanceAt  time.Time               `json:"nextMaintenanceAt"`
	MaintenancePhase   int                     `json:"maintenancePhase,omitempty"`
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
		leaderCtx, leader, release, err := s.acquireBackgroundLease(ctx, "inventory_builder", s.tasks.Leases(), "inventory/builder", requestID(), time.Minute)
		if err == nil {
			err = s.runInventoryLeader(leaderCtx, leader)
			_ = release()
		}
		if err != nil && !errors.Is(err, lease.ErrHeld) && !errors.Is(err, errBackgroundAdmissionYield) && ctx.Err() == nil {
			s.inventoryFailure(err)
			s.inventoryErrorStatus(ctx, err, delay)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if !errors.Is(err, errBackgroundAdmissionYield) {
			delay = min(delay*2, 5*time.Minute)
		}
	}
}

func (s *Service) runInventoryLeader(ctx context.Context, leader lease.Lease) error {
	initial, finish, ok := s.beginBackgroundUnit(ctx, "inventory_builder", 15*time.Second)
	if !ok {
		return errBackgroundAdmissionYield
	}
	checkpoint, _, err := inventory.Read[inventoryCheckpoint](initial, s.store, inventory.Prefix+"checkpoint.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		finish()
		return err
	}
	err = s.claimInventory(initial, leader, &checkpoint)
	finish()
	if err != nil {
		return err
	}
	availability := inventoryAvailability{}
	for ctx.Err() == nil {
		if err := s.checkInventoryAvailability(ctx, leader, &checkpoint, &availability); err != nil {
			return err
		}
		if s.inventoryIdle(&checkpoint) {
			s.maintainInventory(ctx, &checkpoint)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		batchCtx, cancel := context.WithTimeout(ctx, s.inventoryConfig().BatchTimeout)
		s.updateWorker("inventory_builder", func(state *workerStatus) {
			state.State = "execution"
			state.RunID = requestID()
			state.LastStartedAt = time.Now().UTC()
			state.ExecutionBudgetSeconds = s.inventoryConfig().BatchTimeout.Seconds()
		})
		started := time.Now()
		err = s.runInventoryBatch(batchCtx, leader, &checkpoint)
		cancel()
		s.updateWorker("inventory_builder", func(state *workerStatus) {
			state.State = "idle"
			state.LastFinishedAt = time.Now().UTC()
			state.ExecutionDurationSeconds = time.Since(started).Seconds()
		})
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
		if state.ScanStartedAt.IsZero() {
			state.ScanStartedAt = time.Now().UTC()
		}
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
