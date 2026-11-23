package app

import (
	"context"
	"encoding/json"
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
		{"diagnostic_retention_event_budgets", diagnostics.Prefix + "event-budgets/"},
		{"diagnostic_retention_collected", diagnostics.Prefix + "collected/"},
		{"diagnostic_retention_source_leases", "leases/diagnostics/event-outbox/"},
		{"diagnostic_retention_notification_work", diagnostics.Prefix + "notification-work/"},
		{"diagnostic_retention_work_history", terminalWorkHistoryPrefix},
		{"diagnostic_retention_work_history_index", terminalWorkHistoryOrderPrefix},
		{"diagnostic_retention_transfer_history", terminalTransferHistoryPrefix},
		{"diagnostic_retention_transfer_history_index", terminalTransferHistoryOrderPrefix},
		{"diagnostic_retention_transfer_expired", terminalTransferExpiredPrefix},
		{"diagnostic_retention_transfers", diagnostics.Prefix + "transfers/"},
		{"diagnostic_retention_transfer_sessions", diagnostics.Prefix + "transfer-sessions/"},
		{"diagnostic_retention_upload_receipts", diagnostics.Prefix + "upload-receipts/"},
		{"diagnostic_retention_transfer_progress", diagnostics.Prefix + "transfer-progress/"},
		{"diagnostic_retention_storage_snapshots", storageInventoryPrefix + "snapshots/"},
		{"diagnostic_retention_storage_records", storageRetentionPrefix},
	}
	var group sync.WaitGroup
	for _, worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
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
	case strings.HasPrefix(entry.Key, "leases/diagnostics/event-outbox/"):
		return s.cleanupReleasedDiagnosticSourceLease(ctx, entry, p)
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"events/"):
		age = s.diagnosticsConfig().EventRetention
	case strings.HasPrefix(entry.Key, terminalWorkHistoryPrefix):
		record, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if record.TerminalAt.IsZero() || !historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
			return err
		}
		if err := s.releaseHistoryProjectionBudget(ctx, terminalWorkHistoryPrefix, entry.Key, entry.Size); err != nil {
			return err
		}
		incrementSweepCount(p, "deleted", 1)
		return nil
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
		if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
			return err
		}
		incrementSweepCount(p, "deleted", 1)
		return nil
	case strings.HasPrefix(entry.Key, terminalTransferHistoryPrefix):
		record, _, err := inventory.Read[terminalTransferHistoryRecord](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if record.TerminalAt.IsZero() || !historyRecordExpired(record.TerminalAt, time.Now().UTC(), s.diagnosticsConfig().TerminalHistoryRetention) {
			incrementSweepCount(p, "skipped", 1)
			return nil
		}
		if err := s.deleteDiagnosticObservation(ctx, entry); err != nil {
			return err
		}
		if err := s.releaseHistoryProjectionBudget(ctx, terminalTransferHistoryPrefix, entry.Key, entry.Size); err != nil {
			return err
		}
		incrementSweepCount(p, "deleted", 1)
		return nil
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
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"event-budgets/"):
		age = s.diagnosticsConfig().EventBudgetRetention
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"collected/"):
		age = s.diagnosticsConfig().CollectedReceiptRetention
	case strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-sessions/"), strings.HasPrefix(entry.Key, diagnostics.Prefix+"upload-receipts/"), strings.HasPrefix(entry.Key, diagnostics.Prefix+"transfer-progress/"):
		age = s.diagnosticsConfig().TransferObservationRetention
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
		age = s.diagnosticsConfig().TransferObservationRetention
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

func (s *Service) cleanupReleasedDiagnosticSourceLease(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	object, err := s.store.Get(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		incrementSweepCount(p, "deleted", 1)
		return nil
	}
	if err != nil {
		return err
	}
	defer object.Body.Close()
	var state struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(object.Body).Decode(&state); err != nil {
		return storage.ErrCorruptObject
	}
	if state.State != "released" {
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
	err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, headErr := s.store.Head(ctx, entry.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return nil
		}
		if headErr != nil {
			return errors.Join(err, headErr)
		}
		if sourceEntryVersion(current) != sourceEntryVersion(entry) {
			return storage.ErrConditionFailed
		}
	}
	return err
}
