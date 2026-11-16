package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

const storageCollectionQuantumTarget = 8 * time.Second

func (s *Service) storageCollectionQuantum(ctx context.Context, owner lease.Lease, state *storageCollection, registry storage.Store, registryErr error) (maintenanceProgress, error) {
	started, reserve := time.Now(), time.Second
	progress := maintenanceProgress{}
	ctx = context.WithValue(ctx, storageReadCacheKey{}, newStorageReadCache())
	for step := 0; step < 8; step++ {
		if step > 0 && (time.Since(started)+reserve > storageCollectionQuantumTarget || !storageQuantumTime(ctx, reserve)) {
			state.BudgetYields++
			state.LastYieldReason = "execution_budget"
			if err := s.checkpointStorageCollection(ctx, owner, state); err != nil {
				return progress, err
			}
			break
		}
		unit := time.Now()
		phase := storageCollectionPhase(*state)
		if err := s.tasks.Leases().Validate(ctx, owner); err != nil {
			return progress, err
		}
		if state.Domain == 1 && registry == nil && registryErr == nil {
			break
		}
		before, err := json.Marshal(state)
		if err != nil {
			return progress, err
		}
		err = s.advanceStorageCollection(ctx, state, registry, registryErr)
		if err != nil {
			pageSize := state.ListPageSize
			*state = storageCollection{}
			if restoreErr := json.Unmarshal(before, state); restoreErr != nil {
				return progress, restoreErr
			}
			state.ListPageSize, state.ListSuccesses = pageSize, 0
		}
		if err == nil {
			state.ProgressAt = time.Now().UTC()
		}
		if checkpointErr := s.checkpointStorageCollection(ctx, owner, state); checkpointErr != nil {
			return progress, errors.Join(err, checkpointErr)
		}
		if s.metrics != nil && s.metrics.storageCollection != nil {
			s.metrics.storageCollection.WithLabelValues("transition", phase).Observe(time.Since(unit).Seconds())
		}
		if err != nil {
			return progress, err
		}
		progress.EntityProgress, progress.RoundComplete, progress.NextScanAt = true, !state.NextScanAt.IsZero(), state.NextScanAt
		reserve = max(time.Second, 2*time.Since(unit))
		if progress.RoundComplete {
			break
		}
	}
	return progress, nil
}

func storageQuantumTime(ctx context.Context, reserve time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ctx.Err() == nil && (!ok || time.Until(deadline) > reserve)
}

func (s *Service) checkpointStorageCollection(ctx context.Context, owner lease.Lease, state *storageCollection) error {
	started := time.Now()
	defer func() {
		if s.metrics != nil && s.metrics.storageCollection != nil {
			s.metrics.storageCollection.WithLabelValues("checkpoint", "all").Observe(time.Since(started).Seconds())
		}
	}()
	parent, ok := ctx.Value(backgroundParentKey{}).(context.Context)
	if !ok {
		parent = ctx
	}
	bounded, cancel := s.backgroundCheckpoint(parent, "storage_inventory")
	defer cancel()
	bounded = inventory.WithWriteGuard(bounded, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, owner) })
	state.CheckpointAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 && (state.SourceListed || !state.Pending) {
		return storage.ErrCorruptObject
	}
	return inventory.Update(bounded, s.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = *state; return nil })
}

func (s *Service) prepareStoragePage(ctx context.Context, state *storageCollection, store storage.Store, domain string) error {
	if !state.SourceListed {
		return s.listStoragePage(ctx, state, store, domain)
	}
	namespace := diagnostics.ID(domain, s.cfg.StorageBackend, s.cfg.FilesystemRoot, s.cfg.S3.Endpoint, s.cfg.S3.Bucket, s.cfg.S3.Prefix)
	if domain == "registry" {
		namespace = diagnostics.ID(domain, s.cfg.RegistryInspection.Backend, s.cfg.RegistryInspection.FilesystemRoot, s.cfg.RegistryInspection.S3.Endpoint, s.cfg.RegistryInspection.S3.Bucket, s.cfg.RegistryInspection.S3.Prefix)
	}
	rows, size, offset := []storageObject{}, 2, state.SourceOffset
	summary := storageUsage{Domain: domain, Category: "storage_inventory_output", Dimension: "category", Name: "storage_inventory_output", Mask: 1, LogicalUnknown: true, Attribution: "category summary only; internal collection output has no object detail"}
	started := time.Now()
	for offset < len(state.SourceEntries) && len(rows) < 32 {
		if offset > state.SourceOffset && (time.Since(started) > 2*time.Second || !storageQuantumTime(ctx, time.Second)) {
			break
		}
		entry := state.SourceEntries[offset]
		if domain == "state" && strings.HasPrefix(entry.Key, storageInventoryPrefix+"snapshots/"+state.Snapshot.SnapshotID+"/") {
			offset++
			continue
		}
		internalOutput := domain == "state" && strings.HasPrefix(entry.Key, storageInventoryPrefix)
		if internalOutput && storageSummaryOnly(state) {
			summary.ObjectBytes += entry.Size
			summary.Objects++
			offset++
			continue
		}
		var object storageObject
		if internalOutput {
			object = s.inspectStorageObjectMetadata(ctx, domain, namespace, entry)
		} else {
			object = s.inspectStorageObject(ctx, domain, namespace, entry)
		}
		identity := object.Digest
		if domain == "state" && strings.HasPrefix(entry.Key, "cache/objects/") {
			parts := strings.Split(entry.Key, "/")
			if len(parts) > 2 {
				identity = parts[2]
			}
		}
		if identity != "" && (domain == "registry" || strings.HasPrefix(entry.Key, "cache/objects/")) {
			s.attachStorageAssociations(ctx, state.Snapshot.SnapshotID, domain, identity, &object)
		}
		object.ID = fmt.Sprintf("%s.%d.%d.%s", state.Snapshot.SnapshotID, state.Snapshot.Pages, len(rows), object.PhysicalID[:12])
		encoded, err := json.Marshal(object)
		if err != nil {
			return err
		}
		if size+len(encoded)+1 > 1<<20 {
			if len(rows) == 0 {
				return storage.ErrCorruptObject
			}
			break
		}
		size += len(encoded) + 1
		rows, offset = append(rows, object), offset+1
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if offset < len(state.SourceEntries) && time.Since(started) > 2*time.Second {
		state.ListPageSize, state.ListSuccesses = max(1, state.ListPageSize/2), 0
	}
	state.PendingRows, state.PendingNext, state.Pending, state.PendingOffset = rows, state.SourceNext, true, offset
	state.PendingSummary = summary
	return nil
}

func (s *Service) listStoragePage(ctx context.Context, state *storageCollection, store storage.Store, domain string) error {
	if state.ListPageSize == 0 {
		state.ListPageSize = 4
	}
	state.ListPageSize = min(32, max(1, state.ListPageSize))
	started := time.Now()
	entries, next, err := storage.Scan(ctx, store, "", state.Token, state.ListPageSize)
	if s.metrics != nil && s.metrics.storageCollection != nil {
		s.metrics.storageCollection.WithLabelValues("list", domain).Observe(time.Since(started).Seconds())
		s.metrics.storageListSize.WithLabelValues(domain).Observe(float64(state.ListPageSize))
	}
	if err != nil {
		state.ListSuccesses = 0
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrTimeout) {
			state.ListPageSize = max(1, state.ListPageSize/2)
		}
		return err
	}
	state.ListSuccesses++
	if time.Since(started) >= 2*time.Second {
		state.ListSuccesses = 0
	}
	if state.ListSuccesses >= 3 {
		state.ListPageSize, state.ListSuccesses = min(32, state.ListPageSize*2), 0
	}
	if state.EntriesExamined != nil {
		*state.EntriesExamined += int64(len(entries))
	}
	if storageSummaryOnly(state) {
		state.Snapshot.Cost.InputObjects += int64(len(entries))
		for _, entry := range entries {
			state.Snapshot.Cost.InputBytes += entry.Size
		}
	}
	state.SourceEntries, state.SourceNext, state.SourceListed, state.SourceOffset = entries, next, true, 0
	return nil
}
