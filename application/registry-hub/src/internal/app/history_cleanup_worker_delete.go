package app

import (
	"context"
	"strings"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func historyCleanupEntryChanged(listed, current storage.Entry) bool {
	if listed.Version != "" && current.Version != "" {
		return listed.Version != current.Version
	}
	return listed.ModTime != current.ModTime || listed.Size != current.Size
}

func (s *Service) deleteHistoryCleanupEntry(ctx context.Context, category string, entry storage.Entry, cutoff time.Time) (historyCleanupEntryProgress, error) {
	switch category {
	case "diagnostic_events":
		return s.deleteHistoryCleanupObject(ctx, entry)
	case "terminal_work_history", "full_work_receipts":
		if strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/") {
			return s.deleteHistoryCleanupReceipt(ctx, entry)
		}
		outcome, err := s.deleteHistoryCleanupObject(ctx, entry)
		if err == nil && strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix) {
			if releaseErr := s.releaseHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, entry.Key, entry.Size); releaseErr != nil {
				err = releaseErr
			}
		}
		return outcome, err
	case "terminal_transfer_history":
		outcome, err := s.deleteHistoryCleanupObject(ctx, entry)
		if err == nil && strings.HasPrefix(entry.Key, terminalTransferHistoryPrefix) {
			if releaseErr := s.releaseHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, entry.Key, entry.Size); releaseErr != nil {
				err = releaseErr
			}
		}
		return outcome, err
	case "diagnostic_collection_receipts", "transfer_observations":
		return s.deleteHistoryCleanupObject(ctx, entry)
	case "storage_snapshot_history":
		parts := strings.Split(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
		if len(parts) < 2 {
			return historyCleanupEntryProgress{}, storage.ErrCorruptObject
		}
		return s.advanceStorageRetentionVersion(ctx, parts[0], cutoff, entry.Version)
	default:
		return historyCleanupEntryProgress{}, storage.ErrCorruptObject
	}
}

func (s *Service) deleteHistoryCleanupObject(ctx context.Context, entry storage.Entry) (historyCleanupEntryProgress, error) {
	deleted, missing, err := s.deleteHistoryEntryConditional(ctx, entry)
	if err != nil {
		return historyCleanupEntryProgress{}, err
	}
	outcome := historyCleanupEntryProgress{}
	if deleted {
		outcome.Deleted, outcome.DeletedBytes = 1, entry.Size
	} else if missing {
		outcome.Missing = 1
	}
	return outcome, nil
}

func (s *Service) deleteHistoryCleanupReceipt(ctx context.Context, entry storage.Entry) (historyCleanupEntryProgress, error) {
	var scan sourceSweepCheckpoint
	if err := s.cleanupWorkReceipt(ctx, entry, &scan, false); err != nil {
		return historyCleanupEntryProgress{}, err
	}
	if scan.Status.Counts["deleted"] > 0 {
		return historyCleanupEntryProgress{Deleted: 1, DeletedBytes: entry.Size}, nil
	}
	if scan.Status.Counts["missing"] > 0 {
		return historyCleanupEntryProgress{Missing: 1}, nil
	}
	if scan.Status.Counts["protected_version_changed"] > 0 {
		return historyCleanupEntryProgress{}, storage.ErrConditionFailed
	}
	return historyCleanupEntryProgress{Skipped: 1}, nil
}
