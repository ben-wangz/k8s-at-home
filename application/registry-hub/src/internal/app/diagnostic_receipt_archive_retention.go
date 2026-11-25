package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

const diagnosticReceiptArchiveRetentionWorker = "diagnostic_retention_receipt_archives"

func (s *Service) pruneDiagnosticReceiptArchive(ctx context.Context, entry storage.Entry, progress *sourceSweepCheckpoint) error {
	age := s.diagnosticsConfig().FullWorkReceiptRetention
	if entry.ModTime != 0 && time.Since(time.Unix(0, entry.ModTime)) < age {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	archive, version, err := inventory.Read[diagnosticReceiptArchive](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	kind, id, ok := receiptWorkIdentity(archive.SourceKey)
	if !ok || id == "" || strings.Contains(id, "/") || archive.SourceKey != workqueue.ReceiptKey(kind, id) ||
		entry.Key != diagnosticReceiptArchiveKey(archive.SourceKey) {
		return storage.ErrCorruptObject
	}
	if !archive.UpdatedAt.IsZero() && time.Since(archive.UpdatedAt) < age {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	if entry.ModTime == 0 && archive.UpdatedAt.IsZero() {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	if _, err := s.store.Head(ctx, archive.SourceKey); err == nil {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	entry.Version = version
	deleted, missing, err := s.deleteHistoryEntryConditional(ctx, entry)
	if deleted {
		incrementSweepCount(progress, "deleted", 1)
	} else if missing {
		incrementSweepCount(progress, "missing", 1)
	}
	return err
}
