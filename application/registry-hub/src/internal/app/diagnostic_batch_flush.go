package app

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) flushDueDiagnosticBatches(ctx context.Context, now time.Time) error {
	_, err := s.scanDiagnosticPendingBatches(ctx, now)
	return err
}

func (s *Service) scanDiagnosticPendingBatches(ctx context.Context, now time.Time) (time.Time, error) {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	token := ""
	var result error
	var nextDeadline time.Time
	for {
		entries, next, err := storage.Scan(ctx, s.store, diagnosticPendingBatchPrefix, token, 100)
		if err != nil {
			return nextDeadline, errors.Join(result, err)
		}
		for _, entry := range entries {
			partition, _, readErr := inventory.Read[diagnosticPendingPartition](ctx, s.store, entry.Key)
			if readErr != nil {
				result = errors.Join(result, readErr)
				continue
			}
			date, validKey := diagnosticPendingKeyDate(entry.Key)
			if !validKey || partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil {
				result = errors.Join(result, storage.ErrCorruptObject)
				continue
			}
			deadline := partition.Batch.deadline()
			if !deadline.After(now) || partition.Batch.FlushOutcome != nil {
				if err := s.flushDiagnosticPendingPartition(ctx, date, false); err != nil {
					if errors.Is(err, storage.ErrConditionFailed) {
						deadline = now.Add(time.Second)
					} else {
						result = errors.Join(result, err)
					}
				} else {
					deadline = time.Time{}
				}
			}
			if deadline.After(now) && (nextDeadline.IsZero() || deadline.Before(nextDeadline)) {
				nextDeadline = deadline
			}
		}
		if next == "" {
			return nextDeadline, result
		}
		token = next
	}
}

func (s *Service) flushDiagnosticPendingPartition(ctx context.Context, date string, force bool) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	return s.withDiagnosticFencedLease(ctx, diagnosticPartitionLeaseKey(date), func(guarded context.Context) error {
		key := diagnosticPendingBatchKey(date)
		partition, version, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil {
			return storage.ErrCorruptObject
		}
		if !force && partition.Batch.deadline().After(time.Now().UTC()) {
			return nil
		}
		return s.flushDiagnosticPendingPartitionLocked(guarded, date, partition, version)
	})
}

func (s *Service) flushDiagnosticPendingPartitionLocked(ctx context.Context, date string, partition diagnosticPendingPartition, version string) error {
	if partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil ||
		!diagnosticPendingBatchFits(partition.Batch.Events, date) || len(partition.Batch.Sources) == 0 {
		return storage.ErrCorruptObject
	}
	pending := *partition.Batch
	if s.metrics != nil {
		s.metrics.recordDiagnosticWakeLatency("admission_to_flush_start", time.Since(pending.FirstAdmittedAt))
	}
	outcome := pending.FlushOutcome
	if outcome == nil {
		batch, err := (diagnostics.BatchLog{
			Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: s.diagnosticsConfig().EventRetention,
		}).Append(ctx, pending.Events, pending.Critical)
		outcome = &diagnosticPendingOutcome{Disposition: "archived", BatchID: batch.ID, BatchSHA256: batch.ContentSHA256}
		if errors.Is(err, diagnostics.ErrHistoryCapacity) {
			outcome = &diagnosticPendingOutcome{Disposition: "discarded_by_policy", Reason: "batch_budget_capacity"}
		} else if errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
			outcome = &diagnosticPendingOutcome{Disposition: "discarded_by_policy", Reason: "event_history_expired"}
		} else if err != nil {
			return err
		}
		version, err = s.persistDiagnosticPendingOutcome(ctx, date, pending, *outcome)
		if err != nil {
			return err
		}
		pending.FlushOutcome = outcome
	}
	if outcome.Disposition != "archived" && outcome.Disposition != "discarded_by_policy" {
		return storage.ErrCorruptObject
	}
	if outcome.Disposition == "archived" && (outcome.BatchID == "" || outcome.BatchSHA256 == "") {
		return storage.ErrCorruptObject
	}
	if outcome.Disposition == "discarded_by_policy" && outcome.Reason == "" {
		return storage.ErrCorruptObject
	}
	for _, source := range pending.Sources {
		_, found, err := s.diagnosticPendingDisposition(ctx, source)
		if err != nil {
			return err
		}
		if found {
			continue
		}
		disposition := diagnosticEventDisposition{
			SourceKey: source.SourceKey, SourceVersion: source.SourceVersion,
			FirstEventID: source.FirstEventID, LastEventID: source.LastEventID, EventCount: source.EventCount,
			Disposition: outcome.Disposition, BatchID: outcome.BatchID, BatchSHA256: outcome.BatchSHA256,
		}
		if outcome.Disposition == "discarded_by_policy" {
			gapID, gapErr := s.recordDiagnosticOutboxGapRange(ctx, source.SourceKey, source.SourceVersion,
				source.FirstEventID, source.LastEventID, source.EventCount, outcome.Reason)
			if gapErr != nil {
				return gapErr
			}
			disposition.Reason, disposition.GapID = outcome.Reason, gapID
		}
		if err := s.saveDiagnosticPendingDisposition(ctx, source, disposition); err != nil {
			return err
		}
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	deleteErr := s.store.DeleteIfMatch(ctx, diagnosticPendingBatchKey(date), version)
	if errors.Is(deleteErr, storage.ErrUnknownCommit) {
		_, headErr := s.store.Head(ctx, diagnosticPendingBatchKey(date))
		if errors.Is(headErr, storage.ErrNotFound) {
			deleteErr = nil
		} else if headErr != nil {
			deleteErr = errors.Join(deleteErr, headErr)
		}
	}
	if errors.Is(deleteErr, storage.ErrNotFound) {
		deleteErr = nil
	}
	if deleteErr != nil {
		return deleteErr
	}
	for _, source := range pending.Sources {
		s.queueDiagnosticSourceWake(source.SourceKey, time.Now().UTC())
	}
	return nil
}
