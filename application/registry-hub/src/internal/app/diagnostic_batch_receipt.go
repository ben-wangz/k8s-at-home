package app

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) persistDiagnosticPendingOutcome(ctx context.Context, date string, pending diagnosticPendingBatch, outcome diagnosticPendingOutcome) (string, error) {
	key := diagnosticPendingBatchKey(date)
	if err := inventory.Update(ctx, s.store, key, func(current *diagnosticPendingPartition) error {
		if current.SchemaVersion != diagnosticPendingSchemaVersion || current.Date != date || current.Batch == nil ||
			!sameDiagnosticPendingContent(*current.Batch, pending) {
			return storage.ErrConditionFailed
		}
		if current.Batch.FlushIntent == nil || pending.FlushIntent == nil || *current.Batch.FlushIntent != *pending.FlushIntent {
			return storage.ErrConditionFailed
		}
		if outcome.Disposition == "archived" &&
			(current.Batch.FlushIntent.BatchID != outcome.BatchID || current.Batch.FlushIntent.BatchSHA256 != outcome.BatchSHA256) {
			return storage.ErrCorruptObject
		}
		if current.Batch.FlushOutcome != nil {
			if *current.Batch.FlushOutcome != outcome {
				return storage.ErrCorruptObject
			}
			return nil
		}
		current.Batch.FlushOutcome = &outcome
		return nil
	}); err != nil {
		return "", err
	}
	_, version, err := inventory.Read[diagnosticPendingPartition](ctx, s.store, key)
	return version, err
}

func (s *Service) persistDiagnosticPendingFlushIntent(ctx context.Context, date string, pending diagnosticPendingBatch, intent diagnosticPendingFlushIntent) error {
	return inventory.Update(ctx, s.store, diagnosticPendingBatchKey(date), func(current *diagnosticPendingPartition) error {
		if current.SchemaVersion != diagnosticPendingSchemaVersion || current.Date != date || current.Batch == nil ||
			!sameDiagnosticPendingContent(*current.Batch, pending) {
			return storage.ErrConditionFailed
		}
		if current.Batch.FlushOutcome != nil {
			return storage.ErrConditionFailed
		}
		if current.Batch.FlushIntent != nil && *current.Batch.FlushIntent != intent {
			return storage.ErrCorruptObject
		}
		current.Batch.FlushIntent = &intent
		current.Batch.Appendable = false
		// Older writers also honor the expired deadline after rollback.
		current.Batch.OrdinaryDeadline = time.Time{}
		current.Batch.CriticalDeadline = time.Time{}
		return nil
	})
}

func sameDiagnosticPendingContent(left, right diagnosticPendingBatch) bool {
	leftHash, leftErr := diagnosticEventRangeHash(left.Events)
	rightHash, rightErr := diagnosticEventRangeHash(right.Events)
	if leftErr != nil || rightErr != nil || leftHash != rightHash || left.Date != right.Date || left.Critical != right.Critical || len(left.Sources) != len(right.Sources) {
		return false
	}
	for index := range left.Sources {
		if left.Sources[index] != right.Sources[index] {
			return false
		}
	}
	return true
}

func (s *Service) diagnosticPendingDisposition(ctx context.Context, source diagnosticPendingSource) (diagnosticEventDisposition, bool, error) {
	key := diagnostics.Prefix + "collected/" + diagnostics.ID(source.SourceKey) + ".json"
	receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return diagnosticEventDisposition{}, false, nil
	}
	if err != nil {
		return diagnosticEventDisposition{}, false, err
	}
	for _, disposition := range receipt.Dispositions {
		if disposition.SourceVersion == source.SourceVersion && disposition.FirstEventID == source.FirstEventID &&
			disposition.LastEventID == source.LastEventID && disposition.EventCount == source.EventCount {
			return disposition, true, nil
		}
	}
	return diagnosticEventDisposition{}, false, nil
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
