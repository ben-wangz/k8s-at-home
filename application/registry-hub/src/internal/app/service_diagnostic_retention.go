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

func (s *Service) cleanupStorageObservation(ctx context.Context, entry storage.Entry) error {
	parts := strings.Split(strings.TrimPrefix(entry.Key, storageInventoryPrefix+"snapshots/"), "/")
	if len(parts) < 2 || !inventory.ValidID(parts[0]) {
		return nil
	}
	checkpoint, _, err := inventory.Read[storageCollection](ctx, s.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if checkpoint.Snapshot.SnapshotID == parts[0] {
		return nil
	}
	snapshot, _, err := inventory.Read[storageSnapshot](ctx, s.store, storageInventoryPrefix+"snapshots/"+parts[0]+"/manifest.json")
	if errors.Is(err, storage.ErrNotFound) {
		if entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < 24*time.Hour {
			return nil
		}
	} else if err != nil {
		return err
	} else if snapshot.ExpiresAt.After(time.Now()) {
		return nil
	}
	return s.deleteDiagnosticObservation(ctx, entry)
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
