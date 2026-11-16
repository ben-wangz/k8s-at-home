package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) runDiagnosticRetention(ctx context.Context) {
	s.runSourceSweep(ctx, "diagnostic_retention", diagnostics.Prefix, func(ctx context.Context, entry storage.Entry) error {
		age := time.Duration(0)
		switch {
		case strings.HasPrefix(entry.Key, diagnostics.Prefix+"notification-work/"):
			return s.cleanupNotificationIndex(ctx, entry)
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
				return nil
			}
			age = 30 * 24 * time.Hour
		}
		if age == 0 || entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < age {
			return nil
		}
		return s.deleteDiagnosticObservation(ctx, entry)
	})
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
