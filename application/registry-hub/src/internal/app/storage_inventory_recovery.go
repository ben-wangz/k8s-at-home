package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

const storageInventoryAutomaticRestartLimit = 1

func (s *Service) legacyStorageUsageOutputExists(ctx context.Context, state storageCollection) (bool, error) {
	if state.Snapshot.SnapshotID == "" || state.AttributionPhase != 2 ||
		state.Snapshot.UsageVersion == 0 || state.UsagePending != nil {
		return false, nil
	}
	run := storageUsageRun{Stage: "reduce", Index: state.UsageRuns}
	keys := []string{usageRunPage(state.Snapshot.SnapshotID, run, 0), usageRunPrefix(state.Snapshot.SnapshotID, run) + "manifest.json"}
	found := false
	for _, key := range keys {
		_, err := s.store.Head(ctx, key)
		if err == nil {
			found = true
		} else if !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
	}
	return found, nil
}

func (s *Service) replaceStorageCollection(ctx context.Context, owner lease.Lease, current storageCollection, reason string) (storageCollection, error) {
	if current.AutomaticRestarts >= storageInventoryAutomaticRestartLimit {
		return current, fmt.Errorf("storage inventory build %s exceeded automatic restart limit after %s", current.Snapshot.SnapshotID, reason)
	}
	replacement := newStorageCollection()
	replacement.LastCompletedAt = current.LastCompletedAt
	replacement.AutomaticRestarts = current.AutomaticRestarts + 1
	replacement.RecoveredFromBuildID = current.Snapshot.SnapshotID
	replacement.RecoveryReason = reason
	bounded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error {
		return s.tasks.Leases().Validate(ctx, owner)
	})
	err := inventory.Update(bounded, s.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error {
		if value.Snapshot.SnapshotID != current.Snapshot.SnapshotID ||
			value.AttributionPhase != current.AttributionPhase || value.UsageRuns != current.UsageRuns ||
			!reflect.DeepEqual(value.AttributionUsageCursor, current.AttributionUsageCursor) ||
			!reflect.DeepEqual(value.UsagePending, current.UsagePending) {
			return storage.ErrConditionFailed
		}
		*value = replacement
		return nil
	})
	if err != nil {
		return current, err
	}
	return replacement, nil
}
