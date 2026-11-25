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
	_, batchErr, scanErr := s.scanDiagnosticPendingBatches(ctx, now)
	return errors.Join(batchErr, scanErr)
}

func (s *Service) scanDiagnosticPendingBatches(ctx context.Context, now time.Time) (nextDeadline time.Time, batchErr, scanErr error) {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	token := ""
	for {
		entries, next, err := storage.Scan(ctx, s.store, diagnosticPendingBatchPrefix, token, 100)
		if err != nil {
			return nextDeadline, batchErr, errors.Join(scanErr, err)
		}
		for _, entry := range entries {
			date, validKey := diagnosticPendingKeyDate(entry.Key)
			if !validKey {
				scanErr = errors.Join(scanErr, storage.ErrCorruptObject)
				continue
			}
			deadline, err := s.scanDiagnosticPendingDate(ctx, date, now)
			batchErr = errors.Join(batchErr, err)
			if deadline.After(now) && (nextDeadline.IsZero() || deadline.Before(nextDeadline)) {
				nextDeadline = deadline
			}
		}
		if next == "" {
			return nextDeadline, batchErr, scanErr
		}
		token = next
	}
}

func (s *Service) scanDiagnosticPendingDate(ctx context.Context, date string, now time.Time) (time.Time, error) {
	if retryAt := s.diagnosticBatchBackoff.dueAt(date); retryAt.After(time.Now()) {
		return retryAt, nil
	}
	partition, _, err := inventory.Read[diagnosticPendingPartition](ctx, s.store, diagnosticPendingBatchKey(date))
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return time.Time{}, nil
	case err != nil:
		s.deferDiagnosticBatchRetry(date, err)
	case partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Date != date || partition.Batch == nil:
		return time.Time{}, storage.ErrCorruptObject
	case !partition.Batch.readyToFlush(now):
		return partition.Batch.deadline(), nil
	default:
		err = s.flushDiagnosticPendingPartition(ctx, date, false)
		if err == nil {
			return time.Time{}, nil
		}
	}
	deadline := s.diagnosticBatchBackoff.dueAt(date)
	if !deadline.After(now) {
		deadline = time.Now().Add(time.Second)
	}
	var deferred diagnosticPartitionDeferred
	if errors.Is(err, storage.ErrConditionFailed) || errors.As(err, &deferred) {
		return deadline, nil
	}
	return deadline, err
}

func (s *Service) flushDiagnosticPendingPartition(ctx context.Context, date string, force bool) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	return s.withDiagnosticPendingPartition(ctx, date, force, func(guarded context.Context) error {
		key := diagnosticPendingBatchKey(date)
		partition, version, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			s.diagnosticBatchBackoff.clear(date)
			return nil
		}
		if err != nil {
			return err
		}
		if partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Date != date || partition.Batch == nil {
			return storage.ErrCorruptObject
		}
		if !force && !partition.Batch.readyToFlush(time.Now().UTC()) {
			return nil
		}
		return s.flushDiagnosticPendingPartitionLocked(guarded, date, partition, version)
	})
}

func (s *Service) flushDiagnosticPendingPartitionLocked(ctx context.Context, date string, partition diagnosticPendingPartition, version string) error {
	if partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Date != date || partition.Batch == nil ||
		!diagnosticPendingBatchFits(partition.Batch.Events, date) || len(partition.Batch.Sources) == 0 {
		return storage.ErrCorruptObject
	}
	pending := *partition.Batch
	if s.metrics != nil {
		s.metrics.recordDiagnosticWakeLatency("admission_to_flush_start", time.Since(pending.FirstAdmittedAt))
	}
	outcome := pending.FlushOutcome
	var expectedIntent diagnosticPendingFlushIntent
	if pending.FlushIntent != nil || outcome == nil {
		prepared, err := diagnostics.PrepareEventBatch(pending.Events, pending.Critical)
		if err != nil {
			return err
		}
		sourceHash, err := diagnosticPendingSourcesHash(pending.Sources)
		if err != nil {
			return err
		}
		expectedIntent = diagnosticPendingFlushIntent{
			BatchID: prepared.ID, BatchSHA256: prepared.ContentSHA256, SourceRangesSHA256: sourceHash,
		}
		if pending.FlushIntent != nil && *pending.FlushIntent != expectedIntent {
			return storage.ErrCorruptObject
		}
	}
	if outcome == nil {
		if pending.FlushIntent == nil {
			if err := s.persistDiagnosticPendingFlushIntent(ctx, date, pending, expectedIntent); err != nil {
				return err
			}
			pending.FlushIntent = &expectedIntent
		}
		batch, err := (diagnostics.BatchLog{
			Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: s.diagnosticsConfig().EventRetention,
		}).Append(ctx, pending.Events, pending.Critical)
		if err == nil && (batch.ID != pending.FlushIntent.BatchID || batch.ContentSHA256 != pending.FlushIntent.BatchSHA256) {
			return storage.ErrCorruptObject
		}
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
	if outcome.Disposition == "archived" && pending.FlushIntent != nil &&
		(outcome.BatchID != pending.FlushIntent.BatchID || outcome.BatchSHA256 != pending.FlushIntent.BatchSHA256) {
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
	s.diagnosticBatchBackoff.clear(date)
	for _, source := range pending.Sources {
		s.queueDiagnosticSourceWake(source.SourceKey, time.Now().UTC())
	}
	return nil
}
