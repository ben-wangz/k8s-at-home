package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"time"
)

func (s *Service) runDiagnosticRetention(ctx context.Context) {
	workers := []struct{ kind, prefix string }{
		{"diagnostic_retention_events", diagnostics.Prefix + "events/"},
		{"diagnostic_retention_event_budgets", diagnostics.Prefix + "event-budgets/"},
		{"diagnostic_retention_collected", diagnostics.Prefix + "collected/"},
		{"diagnostic_retention_notification_work", diagnostics.Prefix + "notification-work/"},
		{"diagnostic_retention_transfers", diagnostics.Prefix + "transfers/"},
		{"diagnostic_retention_transfer_sessions", diagnostics.Prefix + "transfer-sessions/"},
		{"diagnostic_retention_upload_receipts", diagnostics.Prefix + "upload-receipts/"},
		{"diagnostic_retention_transfer_progress", diagnostics.Prefix + "transfer-progress/"},
		{"diagnostic_retention_storage_snapshots", storageInventoryPrefix + "snapshots/"},
		{"diagnostic_retention_storage_records", storageRetentionPrefix},
		{"diagnostic_retention_legacy", diagnostics.Prefix},
	}
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			s.runSourceSweepWithState(ctx, worker.kind, worker.prefix, func(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
				if worker.kind == "diagnostic_retention_legacy" && strings.Contains(strings.TrimPrefix(entry.Key, diagnostics.Prefix), "/") {
					incrementSweepCount(p, "skipped", 1)
					return nil
				}
				return s.cleanupDiagnosticRetentionEntry(ctx, entry, p)
			}, nil)
		}()
	}
	group.Wait()
}

func (s *Service) cleanupDiagnosticRetentionEntry(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	age := time.Duration(0)
	switch {
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"notification-work/"):
		deleted, err := s.cleanupNotificationIndex(ctx, entry)
		if err != nil {
			return err
		}
		if deleted {
			incrementSweepCount(p, "deleted", 1)
		} else {
			incrementSweepCount(p, "skipped", 1)
		}
		return nil
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"events/"):
		age = diagnostics.Retention
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"event-budgets/"):
		age = diagnostics.Retention + 24*time.Hour
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"collected/"):
		age = 30 * 24 * time.Hour
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-sessions/"), strings.HasPrefix(entry.Key, diagnostics.Prefix+"upload-receipts/"), strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-progress/"):
		age = 30 * 24 * time.Hour
	case strings.HasPrefix(entry.Key, storageInventoryPrefix+"snapshots/"):
		return s.cleanupStorageObservation(ctx, entry)
	case strings.HasPrefix(entry.Key, storageRetentionPrefix):
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, storageRetentionPrefix), ".json")
		if inventory.ValidID(id) {
			return s.advanceStorageRetention(ctx, id)
		}
		return nil
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfers/"):
		view, _, err := inventory.Read[transferView](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if view.TransferState != "complete" && view.TransferState != "cancelled" && view.TransferState != "solidified" {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		age = 30 * 24 * time.Hour
	}
	if age == 0 || entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < age {
		incrementSweepCount(p, "skipped", 1)
		return nil
	}
	if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
		return err
	}
	incrementSweepCount(p, "deleted", 1)
	return nil
}

func (s *Service) deleteDiagnosticObservation(ctx context.Context, entry storage.Entry) error {
	if entry.Version == "" {
		current, err := s.store.Head(ctx, entry.Key)
		if err != nil {
			return err
		}
		if entry.ModTime != current.ModTime {
			return storage.ErrConditionFailed
		}
		entry = current
	}
	return s.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
}
