package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) publishStorageCollection(ctx context.Context, state *storageCollection) error {
	if state.UsageMerge.Phase == "" && state.OutputUsage.Objects > 0 {
		state.Usage[storageUsageKey(state.OutputUsage)] = state.OutputUsage
		if err := s.flushStorageUsage(ctx, state); err != nil {
			return err
		}
		state.OutputUsage = storageUsage{}
		return nil
	}
	if state.Snapshot.UsageVersion > 0 && state.UsageMerge.Phase != "done" {
		return s.advanceStorageUsageMergeUnit(ctx, state)
	}
	if state.Snapshot.CompletedAt.IsZero() {
		if storageSummaryOnly(state) && !state.OutputCostComplete {
			return s.collectStorageOutputCost(ctx, state)
		}
		state.Snapshot.CompletedAt = time.Now().UTC()
		state.Snapshot.ExpiresAt = state.Snapshot.CompletedAt.Add(storageInventoryPreviousRetention)
		bytes, sessions, err := s.downloadManager().Usage(ctx)
		if err == nil {
			state.Snapshot.DownloadReservedBytes = &bytes
			state.Snapshot.DownloadReservations = &sessions
			state.Snapshot.ReservationObservedAt = time.Now().UTC()
		} else {
			state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "reservation_"+diagnosticReason(err))
		}
		for _, usage := range state.Usage {
			state.Snapshot.Usage = append(state.Snapshot.Usage, usage)
		}
		sortStorageUsage(state.Snapshot.Usage, true)
		return nil
	}
	key := storageInventoryPrefix + "snapshots/" + state.Snapshot.SnapshotID + "/manifest.json"
	manifest := state.Snapshot
	manifest.PreviousSnapshotID = ""
	if manifest.ExpiresAt.Equal(manifest.CompletedAt.Add(storageInventoryLatestRetention)) {
		manifest.ExpiresAt = manifest.CompletedAt.Add(storageInventoryPreviousRetention)
	}
	if err := s.inventory.Immutable(ctx, key, manifest); err != nil {
		return err
	}
	if err := inventory.Update(ctx, s.store, storageInventoryPrefix+"current.json", func(value *storageSnapshot) error {
		if value.StartedAt.After(state.Snapshot.StartedAt) {
			return storage.ErrConditionFailed
		}
		if value.SnapshotID == state.Snapshot.SnapshotID {
			return nil
		}
		previousID := value.SnapshotID
		*value = manifest
		value.PreviousSnapshotID = previousID
		value.ExpiresAt = value.CompletedAt.Add(storageInventoryLatestRetention)
		return nil
	}); err != nil {
		return err
	}
	if s.metrics != nil && s.metrics.storageCollection != nil {
		s.metrics.storageCollection.WithLabelValues("publication", "all").Observe(time.Since(state.Snapshot.StartedAt).Seconds())
	}
	*state = storageCollection{NextScanAt: time.Now().Add(storageInventoryRescanInterval), LastCompletedAt: state.Snapshot.CompletedAt}
	return nil
}
