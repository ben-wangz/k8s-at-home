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
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	token := ""
	var result error
	for {
		entries, next, err := storage.Scan(ctx, s.store, diagnosticPendingBatchPrefix, token, 100)
		if err != nil {
			return errors.Join(result, err)
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
			if !partition.Batch.deadline().After(now) {
				if err := s.flushDiagnosticPendingPartition(ctx, date, false); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
					result = errors.Join(result, err)
				}
			}
		}
		if next == "" {
			return result
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
	batch, err := (diagnostics.BatchLog{
		Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: s.diagnosticsConfig().EventRetention,
	}).Append(ctx, pending.Events, pending.Critical)
	gapReason := ""
	if errors.Is(err, diagnostics.ErrHistoryCapacity) {
		gapReason = "batch_budget_capacity"
	} else if errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
		gapReason = "event_history_expired"
	} else if err != nil {
		return err
	}
	for _, source := range pending.Sources {
		disposition := diagnosticEventDisposition{
			SourceKey: source.SourceKey, SourceVersion: source.SourceVersion,
			FirstEventID: source.FirstEventID, LastEventID: source.LastEventID, EventCount: source.EventCount,
			Disposition: "archived", BatchID: batch.ID, BatchSHA256: batch.ContentSHA256,
		}
		if gapReason != "" {
			gapID, gapErr := s.recordDiagnosticOutboxGapRange(ctx, source.SourceKey, source.SourceVersion,
				source.FirstEventID, source.LastEventID, source.EventCount, gapReason)
			if gapErr != nil {
				return gapErr
			}
			disposition.Disposition, disposition.Reason, disposition.GapID = "discarded_by_policy", gapReason, gapID
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

func (s *Service) saveDiagnosticPendingDisposition(ctx context.Context, source diagnosticPendingSource, disposition diagnosticEventDisposition) error {
	key := diagnostics.Prefix + "collected/" + diagnostics.ID(source.SourceKey) + ".json"
	return inventory.Update(ctx, s.store, key, func(current *eventCollectionReceipt) error {
		if source.SourceVersion != "" {
			if current.SourceVersion != "" && current.SourceVersion != source.SourceVersion {
				return storage.ErrConditionFailed
			}
			current.SourceVersion = source.SourceVersion
		}
		for _, previous := range current.Dispositions {
			if previous.SourceVersion == disposition.SourceVersion && previous.FirstEventID == disposition.FirstEventID && previous.LastEventID == disposition.LastEventID {
				if previous.Disposition != disposition.Disposition || previous.BatchID != disposition.BatchID || previous.BatchSHA256 != disposition.BatchSHA256 || previous.GapID != disposition.GapID {
					return storage.ErrCorruptObject
				}
				return nil
			}
		}
		if current.LastEventID != source.PreviousEventID {
			return storage.ErrConditionFailed
		}
		current.Dispositions = appendDisposition(current.Dispositions, disposition)
		current.LastEventID = source.LastEventID
		if disposition.Disposition == "discarded_by_policy" {
			current.Truncated = true
			current.GapIDs = appendUniqueString(current.GapIDs, disposition.GapID)
		}
		current.UpdatedAt = time.Now().UTC()
		return nil
	})
}
