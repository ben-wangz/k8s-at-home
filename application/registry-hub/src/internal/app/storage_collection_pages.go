package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

const storageDetailRowsLimit = 100000
const storageDetailBytesLimit = 128 << 20

func (s *Service) prepareStoragePage(ctx context.Context, state *storageCollection, store storage.Store, domain string) error {
	if !state.SourceListed {
		return s.listStoragePage(ctx, state, store, domain)
	}
	namespace := diagnostics.ID(domain, s.cfg.StorageBackend, s.cfg.FilesystemRoot, s.cfg.S3.Endpoint, s.cfg.S3.Bucket, s.cfg.S3.Prefix)
	if domain == "registry" {
		namespace = diagnostics.ID(domain, s.cfg.RegistryInspection.Backend, s.cfg.RegistryInspection.FilesystemRoot, s.cfg.RegistryInspection.S3.Endpoint, s.cfg.RegistryInspection.S3.Bucket, s.cfg.RegistryInspection.S3.Prefix)
	}
	rows, size, offset := []storageObject{}, 2, state.SourceOffset
	summaries := map[string]storageUsage{}
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
		category := storageCategory(domain, entry.Key)
		if state.DetailLimitReached || state.Snapshot.DetailRows+len(rows) >= storageDetailRowsLimit || state.Snapshot.DetailBytes+int64(size) >= storageDetailBytesLimit {
			state.DetailLimitReached = true
		}
		if state.DetailLimitReached || domain == "state" && storageSummaryOnlyCategory(state, category) {
			if state.DetailLimitReached {
				state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "object_detail_limit")
			}
			summary, ok := summaries[category]
			if !ok {
				summary = storageUsage{Domain: domain, Category: category, Dimension: "category", Name: category, Mask: 1, LogicalUnknown: true, Attribution: "category summary only; per-object detail, attribution, and reclaimability are unknown"}
			}
			summary.ObjectBytes += entry.Size
			summary.Objects++
			summaries[category] = summary
			offset++
			continue
		}
		var object storageObject
		if domain == "state" && strings.HasPrefix(entry.Key, storageInventoryPrefix) {
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
		if state.Snapshot.DetailBytes+int64(size+len(encoded)+1) > storageDetailBytesLimit {
			if len(rows) > 0 {
				break
			}
			state.DetailLimitReached = true
			continue
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
	state.PendingSummaries = nil
	categories := make([]string, 0, len(summaries))
	for category := range summaries {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		state.PendingSummaries = append(state.PendingSummaries, summaries[category])
	}
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
