package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
	"time"
)

const storageInventoryRescanInterval = 15 * time.Minute
const storageInventoryPreviousRetention = 7 * 24 * time.Hour
const storageInventoryLatestRetention = 14 * 24 * time.Hour

func (s *Service) registryInspectionStore(ctx context.Context) (storage.Store, error) {
	switch s.cfg.RegistryInspection.Backend {
	case "filesystem":
		return filesystem.OpenReadOnly(s.cfg.RegistryInspection.FilesystemRoot)
	case "s3":
		store, err := s3store.NewReadOnly(ctx, s.cfg.RegistryInspection.S3)
		if err != nil {
			return nil, err
		}
		s.initializeStorageAttemptObserver(store)
		return newObservedStore(inspectionReadOnlyStore{store}, s.metrics, s.logger), nil
	default:
		return nil, storage.ErrUnavailable
	}
}

func (s *Service) runStorageInventory(ctx context.Context) {
	var registryStore storage.Store
	var registryErr error
	defer func() {
		if registryStore != nil {
			_ = registryStore.Close()
		}
	}()
	s.runPeriodicMaintenance(ctx, "storage_inventory", func(ctx context.Context) (maintenanceProgress, error) {
		leaseCtx, ownership, release, err := s.acquireBackgroundUnitLease(ctx, "storage_inventory", s.tasks.Leases(), "diagnostics/storage-inventory", s.instanceID, 2*time.Minute)
		if err != nil {
			return maintenanceProgress{}, err
		}
		defer release()
		leaseCtx = withStorageReason(leaseCtx, "storage_inventory")
		guarded := inventory.WithWriteGuard(leaseCtx, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, ownership) })
		state, _, err := inventory.Read[storageCollection](guarded, s.store, storageInventoryPrefix+"checkpoint.json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return maintenanceProgress{}, err
		}
		legacyOutput, err := s.legacyStorageUsageOutputExists(guarded, state)
		if err != nil {
			return maintenanceProgress{}, err
		}
		if legacyOutput {
			previousID := state.Snapshot.SnapshotID
			state, err = s.replaceStorageCollection(guarded, ownership, state, "legacy_immutable_usage_output_without_checkpoint")
			if err != nil {
				return maintenanceProgress{}, err
			}
			s.backgroundEvent("storage_inventory", "legacy_usage_batch_rebuild", fmt.Errorf("restarted build %s because its next immutable usage output has no matching checkpoint", previousID))
		}
		if time.Now().Before(state.NextScanAt) {
			return maintenanceProgress{NextScanAt: state.NextScanAt}, nil
		}
		if state.Snapshot.SnapshotID == "" {
			before, err := json.Marshal(state)
			if err != nil {
				return maintenanceProgress{}, err
			}
			completed := state.LastCompletedAt
			state = newStorageCollection()
			state.LastCompletedAt = completed
			if err := s.checkpointStorageCollectionTransition(guarded, ownership, &state, before); err != nil {
				return maintenanceProgress{}, err
			}
		}
		if state.Domain == 1 && registryStore == nil {
			registryStore, registryErr = s.registryInspectionStore(ctx)
		}
		progress, err := s.storageCollectionQuantum(guarded, ownership, &state, registryStore, registryErr)
		if !errors.Is(err, errImmutableBatchMismatch) {
			return progress, err
		}
		previousID := state.Snapshot.SnapshotID
		state, replaceErr := s.replaceStorageCollection(guarded, ownership, state, "immutable_usage_batch_mismatch")
		if replaceErr != nil {
			return progress, replaceErr
		}
		s.backgroundEvent("storage_inventory", "usage_batch_rebuild", fmt.Errorf("restarted build %s after an immutable usage batch mismatch", previousID))
		return maintenanceProgress{EntityProgress: true}, nil
	})
}

func newStorageCollection() storageCollection {
	now := time.Now().UTC()
	state := storageCollection{Snapshot: storageSnapshot{SnapshotID: diagnostics.ID("storage", now.Format(time.RFC3339Nano)), StartedAt: now, ExpiresAt: now.Add(time.Hour), Domains: map[string]storageDomainSummary{}, Reasons: []string{"active_collection_output_excluded_until_next_scan"}, Consistency: "immutable report from non-atomic metadata sweeps; bytes are not underlying disk usage"}, Usage: map[string]storageUsage{}}
	state.Snapshot.UsageVersion = 1
	state.ListPageSize = 4
	state.EntriesExamined, state.AttributionObjects = new(int64), new(int64)
	for _, domain := range []string{"state", "registry"} {
		state.Snapshot.Domains[domain] = storageDomainSummary{Domain: domain, State: "collecting", Categories: map[string]storageUsage{}, Coverage: []string{"visible current objects only", "S3 noncurrent versions, delete markers and multipart uploads are unknown", "filesystem staging and lock files are excluded"}}
	}
	domain := state.Snapshot.Domains["state"]
	domain.SummaryOnlyCategories = []string{"storage_inventory_output", "diagnostic_events", "terminal_work_history", "terminal_transfer_history"}
	domain.Coverage = append(domain.Coverage, "storage_inventory_output contributes category totals without object details or associations", "diagnostic_events contributes LIST metadata totals without per-object detail, attribution, or reclaimability", "terminal_work_history and terminal_transfer_history, including their ordered indexes, contribute LIST metadata totals without per-object detail or attribution")
	state.Snapshot.Domains["state"] = domain
	return state
}

func storagePageKey(snapshot string, page int) string {
	return fmt.Sprintf("%ssnapshots/%s/objects/%08d.json", storageInventoryPrefix, snapshot, page)
}

func (s *Service) advanceStorageCollection(ctx context.Context, state *storageCollection, registryStore storage.Store, registryErr error) error {
	if state.AttributionPhase < 3 {
		return s.advanceStorageAttribution(ctx, state)
	}
	if state.Domain >= 2 {
		return s.publishStorageCollection(ctx, state)
	}
	domain := []string{"state", "registry"}[state.Domain]
	if state.Pending {
		if len(state.PendingRows) > 0 {
			if err := s.inventory.Immutable(ctx, storagePageKey(state.Snapshot.SnapshotID, state.Snapshot.Pages), state.PendingRows); err != nil {
				return err
			}
			for _, object := range state.PendingRows {
				addStorageUsage(state, object)
			}
			if state.Snapshot.UsageVersion > 0 {
				if err := s.flushStorageUsage(ctx, state); err != nil {
					return err
				}
			}
			state.Snapshot.Pages++
			encoded, err := json.Marshal(state.PendingRows)
			if err != nil {
				return err
			}
			state.Snapshot.DetailRows += len(state.PendingRows)
			state.Snapshot.DetailBytes += int64(len(encoded))
		}
		state.SourceOffset = state.PendingOffset
		applyStorageOutputSummary(state)
		if state.Snapshot.UsageVersion > 0 && len(state.Usage) > 0 {
			if err := s.flushStorageUsage(ctx, state); err != nil {
				return err
			}
		}
		state.Pending = false
		state.PendingRows = nil
		if state.SourceListed && state.SourceOffset < len(state.SourceEntries) {
			return nil
		}
		state.Token = state.PendingNext
		state.SourceEntries, state.SourceNext, state.SourceOffset, state.SourceListed = nil, "", 0, false
		if state.Token == "" {
			summary := state.Snapshot.Domains[domain]
			summary.State = "observed"
			if summary.ObjectBytes == nil {
				zero := int64(0)
				summary.ObjectBytes = &zero
				summary.Objects = &zero
			}
			state.Snapshot.Domains[domain] = summary
			state.Domain++
		}
		return nil
	}
	store := s.store
	if domain == "registry" {
		store = registryStore
	}
	if store == nil || domain == "registry" && registryErr != nil {
		if registryErr != nil && s.cfg.RegistryInspection.Backend != "" && s.cfg.RegistryInspection.Backend != "disabled" {
			return registryErr
		}
		summary := state.Snapshot.Domains[domain]
		summary.State = "unavailable"
		summary.Reason = "inspection_not_configured"
		if registryErr != nil && s.cfg.RegistryInspection.Backend != "" && s.cfg.RegistryInspection.Backend != "disabled" {
			summary.Reason = diagnosticReason(registryErr)
		}
		state.Snapshot.Domains[domain] = summary
		state.Domain++
		return nil
	}
	return s.prepareStoragePage(ctx, state, store, domain)
}
