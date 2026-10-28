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
	SchemaVersion     int                 `json:"schemaVersion"`
	Fence             uint64              `json:"fence"`
	Scan              inventoryScan       `json:"scan"`
	Build             *inventory.Snapshot `json:"build"`
	BuildPhase        int                 `json:"buildPhase"`
	BuildToken        string              `json:"buildToken"`
	NextScanAt        time.Time           `json:"nextScanAt"`
	NextMaintenanceAt time.Time           `json:"nextMaintenanceAt"`
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
	checkpoint.SchemaVersion = 1
	checkpoint.Fence = leader.FencingToken
	if checkpoint.Build != nil && checkpoint.Build.BuilderFence != leader.FencingToken {
		checkpoint.Build = nil
		checkpoint.BuildPhase = 0
		checkpoint.BuildToken = ""
	}
	current, _, currentErr := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
	if errors.Is(currentErr, storage.ErrNotFound) || currentErr == nil && !current.ExpiresAt.After(time.Now()) {
		writer := s.inventory.NewWriter(time.Now().UTC(), s.inventoryConfig().SnapshotRetention, leader.FencingToken)
		if err := writer.Publish(ctx, false, []string{"index_backfilling", "untagged_history_not_enumerable"}); err != nil {
			return err
		}
	} else if currentErr != nil {
		return currentErr
	}
	for ctx.Err() == nil {
		if checkpoint.Build == nil && checkpoint.Scan.Phase == 0 && checkpoint.Scan.Token == "" && checkpoint.NextScanAt.After(time.Now()) && !s.inventoryDirty.Load() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
			continue
		}
		batchCtx, cancel := context.WithTimeout(ctx, s.inventoryConfig().BatchTimeout)
		started := time.Now()
		err = s.advanceInventory(batchCtx, leader, &checkpoint)
		cancel()
		if s.metrics != nil && s.metrics.inventory != nil {
			s.metrics.inventory.batch.Observe(time.Since(started).Seconds())
		}
		if err != nil {
			return err
		}
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
	if state.Build == nil && !state.NextMaintenanceAt.After(time.Now()) {
		if err := s.cleanupInventorySnapshots(ctx, ""); err != nil {
			return err
		}
		if err := s.cleanupInventoryWork(ctx); err != nil {
			return err
		}
		state.NextMaintenanceAt = time.Now().Add(time.Second)
	}
	if state.Build != nil {
		if err := s.buildInventoryPage(ctx, state); err != nil {
			if errors.Is(err, storage.ErrConditionFailed) {
				state.Build = nil
				state.BuildPhase = 0
				state.BuildToken = ""
				state.Scan.Complete = false
			} else {
				return err
			}
		}
	} else if state.Scan.Phase > 0 || state.Scan.Token != "" || !state.NextScanAt.After(time.Now()) || s.inventoryDirty.Swap(false) {
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
			state.NextScanAt = time.Now().Add(s.inventoryConfig().RefreshInterval)
		}
	}
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, inventory.Prefix+"checkpoint.json", func(old *inventoryCheckpoint) error {
		if old.Fence > state.Fence {
			return inventory.ErrSuperseded
		}
		*old = *state
		return nil
	})
}

func (s *Service) buildInventoryPage(ctx context.Context, state *inventoryCheckpoint) error {
	writer := s.inventory.ResumeWriter(*state.Build)
	prefixes := []string{"contents/", "bindings/", "cache/"}
	if state.BuildPhase >= len(prefixes) {
		gate, err := s.inventoryEpoch(ctx)
		if err != nil {
			return err
		}
		if writer.Snapshot.GCEpoch != gate.Epoch {
			return storage.ErrConditionFailed
		}
		if err := writer.Publish(ctx, state.Scan.Complete, state.Scan.Reasons); err != nil {
			return err
		}
		state.Build = nil
		state.Scan.Reasons = nil
		if s.metrics != nil {
			s.metrics.recordEvent("inventory_snapshot_published")
			s.metrics.inventoryPublished(writer.Snapshot)
		}
		return s.cleanupInventorySnapshots(ctx, writer.Snapshot.SnapshotID)
	}
	entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+prefixes[state.BuildPhase], state.BuildToken, s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch state.BuildPhase {
		case 0:
			err = s.buildInventoryContent(ctx, writer, entry.Key)
		case 1:
			err = s.buildInventoryReference(ctx, writer, entry.Key)
		case 2:
			err = s.buildInventoryCacheObject(ctx, writer, entry.Key)
		}
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
	}
	if err := writer.Flush(ctx); err != nil {
		return err
	}
	state.Build = &writer.Snapshot
	state.BuildToken = next
	if next == "" {
		state.BuildPhase++
		state.BuildToken = ""
	}
	return nil
}
