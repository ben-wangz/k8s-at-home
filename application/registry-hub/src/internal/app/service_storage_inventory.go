package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/storage/s3store"
	"strings"
	"time"
)

func (s *Service) registryInspectionStore(ctx context.Context) (storage.Store, error) {
	switch s.cfg.RegistryInspection.Backend {
	case "filesystem":
		return filesystem.OpenReadOnly(s.cfg.RegistryInspection.FilesystemRoot)
	case "s3":
		store, err := s3store.New(ctx, s.cfg.RegistryInspection.S3)
		if err != nil {
			return nil, err
		}
		return inspectionReadOnlyStore{store}, nil
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
		guarded := inventory.WithWriteGuard(leaseCtx, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, ownership) })
		state, _, err := inventory.Read[storageCollection](guarded, s.store, storageInventoryPrefix+"checkpoint.json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return maintenanceProgress{}, err
		}
		if time.Now().Before(state.NextScanAt) {
			return maintenanceProgress{}, nil
		}
		if state.Snapshot.SnapshotID == "" {
			state = newStorageCollection()
		}
		if state.Domain == 1 && registryStore == nil {
			registryStore, registryErr = s.registryInspectionStore(ctx)
		}
		if err := s.advanceStorageCollection(guarded, &state, registryStore, registryErr); err != nil {
			return maintenanceProgress{}, err
		}
		parent, ok := ctx.Value(backgroundParentKey{}).(context.Context)
		if !ok {
			parent = ctx
		}
		checkpointCtx, cancel := s.backgroundCheckpoint(parent, "storage_inventory")
		defer cancel()
		checkpointCtx = inventory.WithWriteGuard(checkpointCtx, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, ownership) })
		err = inventory.Update(checkpointCtx, s.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil })
		return maintenanceProgress{EntityProgress: true, RoundComplete: !state.NextScanAt.IsZero()}, err
	})
}

func newStorageCollection() storageCollection {
	now := time.Now().UTC()
	state := storageCollection{Snapshot: storageSnapshot{SnapshotID: diagnostics.ID("storage", now.Format(time.RFC3339Nano)), StartedAt: now, ExpiresAt: now.Add(time.Hour), Domains: map[string]storageDomainSummary{}, Reasons: []string{"active_collection_output_excluded_until_next_scan"}, Consistency: "immutable report from non-atomic metadata sweeps; bytes are not underlying disk usage"}, Usage: map[string]storageUsage{}}
	state.Snapshot.UsageVersion = 1
	for _, domain := range []string{"state", "registry"} {
		state.Snapshot.Domains[domain] = storageDomainSummary{Domain: domain, State: "collecting", Categories: map[string]storageUsage{}, Coverage: []string{"visible current objects only", "S3 noncurrent versions, delete markers and multipart uploads are unknown", "filesystem staging and lock files are excluded"}}
	}
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
		}
		state.Token = state.PendingNext
		state.Pending = false
		state.PendingRows = nil
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
	entries, next, err := storage.Scan(ctx, store, "", state.Token, 4)
	if err != nil {
		if domain == "registry" {
			summary := state.Snapshot.Domains[domain]
			summary.State, summary.Reason = "partial", diagnosticReason(err)
			state.Snapshot.Domains[domain] = summary
			state.Domain++
			return nil
		}
		return err
	}
	namespace := diagnostics.ID(domain, s.cfg.StorageBackend, s.cfg.FilesystemRoot, s.cfg.S3.Endpoint, s.cfg.S3.Bucket, s.cfg.S3.Prefix)
	if domain == "registry" {
		namespace = diagnostics.ID(domain, s.cfg.RegistryInspection.Backend, s.cfg.RegistryInspection.FilesystemRoot, s.cfg.RegistryInspection.S3.Endpoint, s.cfg.RegistryInspection.S3.Bucket, s.cfg.RegistryInspection.S3.Prefix)
	}
	rows := []storageObject{}
	for _, entry := range entries {
		if domain == "state" && strings.HasPrefix(entry.Key, storageInventoryPrefix+"snapshots/"+state.Snapshot.SnapshotID+"/") {
			continue
		}
		object := s.inspectStorageObject(ctx, domain, namespace, entry)
		if domain == "registry" && object.Digest != "" {
			s.attachStorageAssociations(ctx, state.Snapshot.SnapshotID, domain, object.Digest, &object)
		}
		if domain == "state" && strings.HasPrefix(entry.Key, "cache/objects/") {
			parts := strings.Split(entry.Key, "/")
			if len(parts) > 2 {
				s.attachStorageAssociations(ctx, state.Snapshot.SnapshotID, domain, parts[2], &object)
			}
		}
		object.ID = fmt.Sprintf("%s.%d.%d.%s", state.Snapshot.SnapshotID, state.Snapshot.Pages, len(rows), object.PhysicalID[:12])
		rows = append(rows, object)
	}
	state.PendingRows, state.PendingNext, state.Pending = rows, next, true
	return nil
}

func (s *Service) publishStorageCollection(ctx context.Context, state *storageCollection) error {
	if state.Snapshot.UsageVersion > 0 && state.UsageMerge.Phase != "done" {
		return s.advanceStorageUsageMerge(ctx, state)
	}
	if state.Snapshot.CompletedAt.IsZero() {
		state.Snapshot.CompletedAt = time.Now().UTC()
		state.Snapshot.ExpiresAt = state.Snapshot.CompletedAt.Add(time.Hour)
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
	if err := s.inventory.Immutable(ctx, key, state.Snapshot); err != nil {
		return err
	}
	if err := inventory.Update(ctx, s.store, storageInventoryPrefix+"current.json", func(value *storageSnapshot) error {
		if value.StartedAt.After(state.Snapshot.StartedAt) {
			return storage.ErrConditionFailed
		}
		*value = state.Snapshot
		return nil
	}); err != nil {
		return err
	}
	*state = storageCollection{NextScanAt: time.Now().Add(5 * time.Minute)}
	return nil
}
