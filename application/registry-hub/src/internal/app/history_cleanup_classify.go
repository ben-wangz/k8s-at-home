package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

func (s *Service) classifyHistoryCleanupEntry(ctx context.Context, category string, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	switch category {
	case "diagnostic_events":
		date, ok := historyCleanupEventDate(entry.Key)
		if !ok {
			return historyCleanupCandidate{Reason: "unknown_event_partition"}, nil
		}
		return historyCleanupCandidate{Eligible: date < cutoff.Format("20060102"), Reason: "unsealed_partition"}, nil
	case "terminal_work_history":
		return s.classifyTerminalWorkEntry(ctx, entry, cutoff)
	case "terminal_transfer_history":
		return s.classifyTerminalTransferEntry(ctx, entry, cutoff)
	case "storage_snapshot_history":
		return s.classifyStorageSnapshotEntry(ctx, entry, cutoff)
	case "full_work_receipts":
		return s.classifyWorkReceiptEntry(ctx, entry, cutoff)
	case "diagnostic_collection_receipts":
		return s.classifyCollectionReceiptEntry(ctx, entry, cutoff)
	case "transfer_observations":
		return s.classifyTransferObservationEntry(ctx, entry, cutoff)
	default:
		return historyCleanupCandidate{Reason: "unsupported_category"}, nil
	}
}

func (s *Service) classifyTerminalWorkEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	switch {
	case strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix):
		record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, entry.Key)
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		if !validTerminalWorkHistory(record, entry.Key) {
			return historyCleanupCandidate{Reason: "invalid_history_record"}, nil
		}
		return historyCleanupCandidate{Eligible: !record.TerminalAt.After(cutoff), Reason: "after_cutoff"}, nil
	case strings.HasPrefix(entry.Key, terminalWorkHistoryOrderPrefix):
		at, ok := historyOrderTime(terminalWorkHistoryOrderPrefix, entry.Key)
		return historyCleanupCandidate{Eligible: ok && !at.After(cutoff), Reason: "after_cutoff"}, nil
	case strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/"):
		return s.classifyWorkReceiptEntry(ctx, entry, cutoff)
	default:
		return historyCleanupCandidate{Reason: "outside_history_scope"}, nil
	}
}

func (s *Service) classifyWorkReceiptEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	_, id, ok := receiptWorkIdentity(entry.Key)
	if !ok {
		return historyCleanupCandidate{Reason: "invalid_receipt_key"}, nil
	}
	receipt, _, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupCandidate{Reason: "receipt_missing"}, nil
	}
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if receipt.ID != id || receipt.State != "done" {
		return historyCleanupCandidate{Reason: "active_receipt"}, nil
	}
	terminalAt := receipt.CompletedAt
	if terminalAt.IsZero() && entry.ModTime != 0 {
		terminalAt = time.Unix(0, entry.ModTime).UTC()
	}
	if terminalAt.IsZero() || terminalAt.After(cutoff) {
		return historyCleanupCandidate{Reason: "after_cutoff"}, nil
	}
	if time.Since(terminalAt) < s.diagnosticsConfig().FullWorkReceiptRetention {
		return historyCleanupCandidate{Reason: "minimum_receipt_retention"}, nil
	}
	var progress sourceSweepCheckpoint
	if err := s.cleanupWorkReceipt(ctx, entry, &progress, true); err != nil {
		return historyCleanupCandidate{}, err
	}
	if progress.Status.Counts["eligible"] > 0 {
		return historyCleanupCandidate{Eligible: true}, nil
	}
	for _, reason := range []string{"protected_active_work", "protected_execution_sources_unverified", "protected_execution_fact_pending", "protected_outbox_pending", "protected_version_changed"} {
		if progress.Status.Counts[reason] > 0 {
			return historyCleanupCandidate{Reason: reason}, nil
		}
	}
	return historyCleanupCandidate{Reason: "receipt_not_eligible"}, nil
}

func (s *Service) classifyTerminalTransferEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	if strings.HasPrefix(entry.Key, terminalTransferHistoryOrderPrefix) {
		at, ok := historyOrderTime(terminalTransferHistoryOrderPrefix, entry.Key)
		return historyCleanupCandidate{Eligible: ok && !at.After(cutoff), Reason: "after_cutoff"}, nil
	}
	record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, entry.Key)
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if !validTerminalTransferHistory(record, entry.Key) {
		return historyCleanupCandidate{Reason: "invalid_history_record"}, nil
	}
	return historyCleanupCandidate{Eligible: !record.TerminalAt.After(cutoff), Reason: "after_cutoff"}, nil
}

func (s *Service) classifyStorageSnapshotEntry(ctx context.Context, entry storage.Entry, cutoff time.Time) (historyCleanupCandidate, error) {
	if !strings.HasSuffix(entry.Key, "/manifest.json") {
		return historyCleanupCandidate{Reason: "snapshot_manifest_only"}, nil
	}
	parts := strings.Split(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	if len(parts) < 2 || !inventory.ValidID(parts[0]) {
		return historyCleanupCandidate{Reason: "invalid_snapshot_key"}, nil
	}
	if active, err := s.storageCollectionActive(ctx, parts[0]); err != nil || active {
		if err != nil {
			return historyCleanupCandidate{}, err
		}
		return historyCleanupCandidate{Reason: "active_snapshot"}, nil
	}
	snapshot, _, err := inventory.Read[storageSnapshot](ctx, s.store, storageInventoryPrefix+"snapshots/"+parts[0]+"/manifest.json")
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupCandidate{Reason: "snapshot_manifest_missing"}, nil
	}
	if err != nil {
		return historyCleanupCandidate{}, err
	}
	if snapshot.SnapshotID != parts[0] || snapshot.ExpiresAt.IsZero() {
		return historyCleanupCandidate{Reason: "invalid_snapshot_manifest"}, nil
	}
	return historyCleanupCandidate{Eligible: !snapshot.ExpiresAt.After(cutoff), Reason: "live_snapshot"}, nil
}
