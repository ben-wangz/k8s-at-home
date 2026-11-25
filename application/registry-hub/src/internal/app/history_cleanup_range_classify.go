package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) classifyHistoryCleanupRangeEntry(ctx context.Context, preview historyCleanupRangePreview, entry storage.Entry, progress *historyCleanupRangeProgress) (historyCleanupRangeCandidate, bool, error) {
	switch preview.Category {
	case "diagnostic_events":
		return classifyHistoryCleanupEvent(ctx, s.store, entry, preview.EffectiveRange)
	case "terminal_work_history":
		return s.classifyHistoryCleanupWorkIndex(ctx, entry, preview.EffectiveRange)
	case "terminal_transfer_history":
		return s.classifyHistoryCleanupTransferIndex(ctx, entry, preview.EffectiveRange)
	case "storage_snapshot_history":
		return s.classifyHistoryCleanupSnapshot(ctx, entry, preview.EffectiveRange, progress)
	case "full_work_receipts":
		return s.classifyHistoryCleanupFullReceipt(ctx, entry, preview.EffectiveRange)
	case "diagnostic_collection_receipts":
		return s.classifyHistoryCleanupCollectionReceipt(ctx, entry, preview.EffectiveRange)
	case "transfer_observations":
		return s.classifyHistoryCleanupTransferObservation(ctx, entry, preview.EffectiveRange)
	default:
		return historyCleanupRangeCandidate{}, false, storage.ErrCorruptObject
	}
}

func classifyHistoryCleanupEvent(ctx context.Context, store storage.Store, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	if strings.HasPrefix(entry.Key, diagnostics.BatchPrefix) {
		batch, _, err := inventory.Read[diagnostics.EventBatch](ctx, store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return historyCleanupRangeCandidate{}, false, nil
		}
		if err != nil {
			return historyCleanupRangeCandidate{}, false, err
		}
		if err := diagnostics.VerifyEventBatch(batch, entry.Key); err != nil {
			return historyCleanupRangeCandidate{Reason: "invalid_event_batch"}, true, nil
		}
		inRange, allInRange := false, true
		for _, event := range batch.Events {
			matched := historyCleanupTimeInRange(event.OccurredAt, scope)
			inRange = inRange || matched
			allInRange = allInRange && matched
		}
		if !inRange {
			return historyCleanupRangeCandidate{}, false, nil
		}
		if !allInRange {
			return historyCleanupRangeCandidate{Reason: "cross_range_batch"}, true, nil
		}
		return eligibleHistoryCleanupRangeEntry(entry, batch.Events[0].OccurredAt)
	}
	event, _, err := inventory.Read[diagnostics.Event](ctx, store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if event.ID == "" || event.OccurredAt.IsZero() || diagnostics.EventKey(event) != entry.Key {
		return historyCleanupRangeCandidate{Reason: "invalid_event_record"}, true, nil
	}
	if !historyCleanupTimeInRange(event.OccurredAt, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	return eligibleHistoryCleanupRangeEntry(entry, event.OccurredAt)
}

func (s *Service) classifyHistoryCleanupWorkIndex(ctx context.Context, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	at, ok := historyOrderTime(terminalWorkHistoryOrderPrefix, entry.Key)
	if !ok || !historyCleanupTimeInRange(at, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	kind, id, ok := parseHistoryOrderKey(terminalWorkHistoryOrderPrefix, entry.Key)
	if !ok {
		return historyCleanupRangeCandidate{Reason: "invalid_history_index"}, true, nil
	}
	key := terminalWorkHistoryKey(kind, id)
	record, version, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return eligibleHistoryCleanupRangeEntry(entry, at)
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if !validTerminalWorkHistory(record, key) || !record.TerminalAt.Equal(at) ||
		historyOrderKey(terminalWorkHistoryOrderPrefix, record.TerminalAt, record.Kind, record.ID, record.Revision) != entry.Key {
		return historyCleanupRangeCandidate{Reason: "invalid_history_record"}, true, nil
	}
	return s.historyCleanupIndexedProjection(ctx, entry, key, version, at)
}

func (s *Service) classifyHistoryCleanupTransferIndex(ctx context.Context, entry storage.Entry, scope historyCleanupRange) (historyCleanupRangeCandidate, bool, error) {
	at, ok := historyOrderTime(terminalTransferHistoryOrderPrefix, entry.Key)
	if !ok || !historyCleanupTimeInRange(at, scope) {
		return historyCleanupRangeCandidate{}, false, nil
	}
	_, id, ok := parseHistoryOrderKey(terminalTransferHistoryOrderPrefix, entry.Key)
	if !ok {
		return historyCleanupRangeCandidate{Reason: "invalid_history_index"}, true, nil
	}
	key := terminalTransferHistoryKey(id)
	record, version, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return eligibleHistoryCleanupRangeEntry(entry, at)
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if !validTerminalTransferHistory(record, key) || !record.TerminalAt.Equal(at) ||
		historyOrderKey(terminalTransferHistoryOrderPrefix, record.TerminalAt, record.View.Kind, record.ID, record.Revision) != entry.Key {
		return historyCleanupRangeCandidate{Reason: "invalid_history_record"}, true, nil
	}
	return s.historyCleanupIndexedProjection(ctx, entry, key, version, at)
}

func (s *Service) historyCleanupIndexedProjection(ctx context.Context, entry storage.Entry, key, version string, at time.Time) (historyCleanupRangeCandidate, bool, error) {
	projection, err := s.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return eligibleHistoryCleanupRangeEntry(entry, at)
	}
	if err != nil {
		return historyCleanupRangeCandidate{}, false, err
	}
	if projection.Version != version {
		return historyCleanupRangeCandidate{Reason: "version_changed"}, true, nil
	}
	ref := historyCleanupRef(projection)
	ref.At = at
	index := historyCleanupRef(entry)
	ref.Index = &index
	return historyCleanupRangeCandidate{Eligible: true, Reference: ref}, true, nil
}

func (s *Service) classifyHistoryCleanupSnapshot(ctx context.Context, entry storage.Entry, scope historyCleanupRange, progress *historyCleanupRangeProgress) (historyCleanupRangeCandidate, bool, error) {
	id, child, ok := strings.Cut(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	if !ok || child == "" || !inventory.ValidID(id) {
		return historyCleanupRangeCandidate{Reason: "invalid_snapshot_key"}, true, nil
	}
	if progress.SnapshotID != id {
		snapshot, err := s.readHistoryCleanupSnapshot(ctx, id, scope)
		if err != nil {
			return historyCleanupRangeCandidate{}, false, err
		}
		progress.SnapshotID, progress.Snapshot = id, snapshot
	}
	snapshot := progress.Snapshot
	if snapshot.Outside {
		return historyCleanupRangeCandidate{}, false, nil
	}
	if snapshot.Reason != "" {
		return historyCleanupRangeCandidate{Reason: snapshot.Reason}, true, nil
	}
	if child == "manifest.json" {
		progress.Snapshot.Pending = true
		return historyCleanupRangeCandidate{}, false, nil
	}
	ref := historyCleanupRef(entry)
	ref.Manifest = snapshot.Manifest
	ref.At = snapshot.Manifest.At
	return historyCleanupRangeCandidate{Eligible: true, Reference: ref}, true, nil
}

func (s *Service) readHistoryCleanupSnapshot(ctx context.Context, id string, scope historyCleanupRange) (historyCleanupSnapshotScan, error) {
	if active, err := s.storageCollectionProtected(ctx, id); active || err != nil {
		return historyCleanupSnapshotScan{Reason: "protected_snapshot"}, err
	}
	key := storageInventoryPrefix + "snapshots/" + id + "/manifest.json"
	snapshot, version, err := inventory.Read[storageSnapshot](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupSnapshotScan{Reason: "snapshot_manifest_missing"}, nil
	}
	if err != nil {
		return historyCleanupSnapshotScan{}, err
	}
	if snapshot.SnapshotID != id || snapshot.ExpiresAt.IsZero() {
		return historyCleanupSnapshotScan{Reason: "invalid_snapshot_manifest"}, nil
	}
	expiresAt, err := s.storageSnapshotExpiry(ctx, snapshot)
	if err != nil {
		return historyCleanupSnapshotScan{}, err
	}
	if !historyCleanupTimeInRange(expiresAt, scope) {
		return historyCleanupSnapshotScan{Outside: true}, nil
	}
	entry, err := s.store.Head(ctx, key)
	if err != nil {
		return historyCleanupSnapshotScan{}, err
	}
	if entry.Version != version {
		return historyCleanupSnapshotScan{Reason: "version_changed"}, nil
	}
	ref := historyCleanupRef(entry)
	ref.At = expiresAt
	return historyCleanupSnapshotScan{Manifest: &ref}, nil
}
