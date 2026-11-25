package app

import (
	"context"
	"errors"
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
		{"diagnostic_retention_event_batches", diagnostics.BatchPrefix},
		{diagnosticEventWatermarkWorker, diagnostics.EventHistorySealPrefix},
		{"diagnostic_retention_event_budgets", diagnostics.Prefix + "event-budgets/"},
		{"diagnostic_retention_collected", diagnostics.Prefix + "collected/"},
		{diagnosticReceiptArchiveRetentionWorker, diagnosticReceiptArchivePrefix},
		{"diagnostic_retention_source_leases", "leases/diagnostics/event-outbox/"},
		{"diagnostic_retention_event_day_leases", "leases/diagnostics/event-history-day/"},
		{"diagnostic_retention_notification_work", diagnostics.Prefix + "notification-work/"},
		{terminalWorkHistoryRetentionWorker, terminalWorkHistoryPrefix},
		{"diagnostic_retention_work_history_index", terminalWorkHistoryOrderPrefix},
		{terminalTransferHistoryRetentionWorker, terminalTransferHistoryPrefix},
		{"diagnostic_retention_transfer_history_index", terminalTransferHistoryOrderPrefix},
		{"diagnostic_retention_transfer_expired", terminalTransferExpiredPrefix},
		{"diagnostic_retention_transfers", diagnostics.Prefix + "transfers/"},
		{"diagnostic_retention_transfer_sessions", diagnostics.Prefix + "transfer-sessions/"},
		{"diagnostic_retention_upload_receipts", diagnostics.Prefix + "upload-receipts/"},
		{"diagnostic_retention_transfer_progress", diagnostics.Prefix + "transfer-progress/"},
		{"diagnostic_retention_storage_snapshots", storageInventoryPrefix + "snapshots/"},
		{"diagnostic_retention_storage_records", storageRetentionPrefix},
		{"diagnostic_retention_cleanup_previews", historyCleanupRangePrefix},
		{"diagnostic_retention_cleanup_continuations", historyCleanupContinuationPrefix},
	}
	var group sync.WaitGroup
	for _, worker := range historyCleanupRecordWorkers {
		group.Go(func() {
			s.runSourceSweep(ctx, worker.kind, worker.prefix, func(ctx context.Context, entry storage.Entry) error {
				return s.pruneHistoryCleanupRecord(ctx, worker.kind, entry)
			})
		})
	}
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			if terminalHistoryIndexBackfillWorker(worker.kind) {
				s.runSourceSweepWithState(ctx, worker.kind, worker.prefix, s.sweepTerminalHistory, s.finishTerminalHistoryIndexBackfill(worker.kind))
				return
			}
			s.runSourceSweepWithState(ctx, worker.kind, worker.prefix, func(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
				return s.cleanupDiagnosticRetentionEntry(ctx, entry, p)
			}, nil)
		}()
	}
	group.Wait()
}

func (s *Service) cleanupDiagnosticRetentionEntry(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	age := time.Duration(0)
	switch {
	case strings.HasPrefix(entry.Key, diagnostics.EventHistorySealPrefix):
		date, ok := strings.CutSuffix(strings.TrimPrefix(entry.Key, diagnostics.EventHistorySealPrefix), ".json")
		if !ok {
			return storage.ErrCorruptObject
		}
		return diagnostics.PublishSealedEventDay(ctx, s.store, date)
	case strings.HasPrefix(entry.Key, historyCleanupRangePrefix):
		return s.pruneHistoryCleanupRangeEntry(ctx, entry)
	case strings.HasPrefix(entry.Key, historyCleanupContinuationPrefix):
		return s.pruneHistoryCleanupContinuationEntry(ctx, entry)
	case strings.HasPrefix(entry.Key, diagnosticReceiptArchivePrefix):
		return s.pruneDiagnosticReceiptArchive(ctx, entry, p)
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
	case strings.HasPrefix(entry.Key, "leases/diagnostics/event-outbox/"), strings.HasPrefix(entry.Key, "leases/diagnostics/event-history-day/"):
		return s.cleanupDiagnosticSourceLease(ctx, entry, p)
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"events/"):
		age = s.diagnosticsConfig().EventRetention
		sealed, err := s.sealExpiredDiagnosticEventDay(ctx, entry)
		if err != nil {
			return err
		}
		if !sealed {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
	case strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix):
		record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			if err := s.releaseHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, entry.Key, entry.Size); err != nil {
				return err
			}
			incrementSweepCount(p, "missing", 1)
			return nil
		}
		if err != nil {
			return err
		}
		if record.TerminalAt.IsZero() || !historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		return s.expireTerminalHistoryProjection(ctx, entry, terminalWorkHistoryPrefix, p)
	case strings.HasPrefix(entry.Key, terminalWorkHistoryOrderPrefix), strings.HasPrefix(entry.Key, terminalTransferHistoryOrderPrefix):
		prefix := terminalWorkHistoryOrderPrefix
		if strings.HasPrefix(entry.Key, terminalTransferHistoryOrderPrefix) {
			prefix = terminalTransferHistoryOrderPrefix
		}
		terminalAt, ok := historyOrderTime(prefix, entry.Key)
		if !ok {
			return storage.ErrCorruptObject
		}
		if !historyRecordExpired(terminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		if linked, err := s.terminalHistoryIndexLinked(ctx, prefix, entry.Key); err != nil || linked {
			if linked {
				incrementSweepCount(p, "skipped", 1)
			}
			return err
		}
		if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
			return err
		}
		incrementSweepCount(p, "deleted", 1)
		return nil
	case strings.HasPrefix(entry.Key, terminalTransferHistoryPrefix):
		record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			if err := s.releaseHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, entry.Key, entry.Size); err != nil {
				return err
			}
			incrementSweepCount(p, "missing", 1)
			return nil
		}
		if err != nil {
			return err
		}
		if record.TerminalAt.IsZero() || !historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		return s.expireTerminalHistoryProjection(ctx, entry, terminalTransferHistoryPrefix, p)
	case strings.HasPrefix(entry.Key, terminalTransferExpiredPrefix):
		record, _, err := inventory.Read[terminalTransferExpiry](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if record.ExpiresAt.IsZero() || record.ExpiresAt.After(time.Now().UTC()) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
			return err
		}
		incrementSweepCount(p, "deleted", 1)
		return nil
	case strings.HasPrefix(entry.Key, diagnostics.BatchPrefix):
		age = s.diagnosticsConfig().EventRetention
		sealed, err := s.sealExpiredDiagnosticEventDay(ctx, entry)
		if err != nil {
			return err
		}
		if !sealed {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"event-budgets/"):
		age = s.diagnosticsConfig().EventBudgetRetention
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"collected/"):
		return s.cleanupLegacyHistoryObservation(ctx, entry, p, s.diagnosticsConfig().CollectedReceiptRetention, s.classifyCollectionReceiptEntry)
	case strings.HasPrefix(entry.Key, storageInventoryPrefix+"snapshots/"):
		return s.cleanupStorageObservation(ctx, entry)
	case strings.HasPrefix(entry.Key, storageRetentionPrefix):
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, storageRetentionPrefix), ".json")
		if inventory.ValidID(id) {
			return s.advanceStorageRetention(ctx, id)
		}
		return nil
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfers/"),
		strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-sessions/"),
		strings.HasPrefix(entry.Key, diagnostics.Prefix+"upload-receipts/"),
		strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-progress/"):
		return s.cleanupLegacyHistoryObservation(ctx, entry, p, s.diagnosticsConfig().TransferObservationRetention, s.classifyTransferObservationEntry)
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
