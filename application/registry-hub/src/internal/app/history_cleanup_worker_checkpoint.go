package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) saveHistoryCleanupOperation(ctx context.Context, identity historyCleanupOperation, progress historyCleanupProgress, complete bool) error {
	key := historyCleanupOperationKey(identity.ID)
	if err := inventory.Update(ctx, s.store, key, func(operation *historyCleanupOperation) error {
		if operation.SchemaVersion != 1 || operation.ID != identity.ID || operation.PreviewID != identity.PreviewID || operation.Principal != identity.Principal {
			return storage.ErrCorruptObject
		}
		operation.Counts = progress.Counts
		operation.SegmentOffset = progress.SegmentOffset
		operation.SegmentStarted = progress.SegmentStarted
		operation.SegmentPrimarySettled, operation.SegmentIndexStarted = progress.SegmentPrimarySettled, progress.SegmentIndexStarted
		if operation.SegmentID != "" {
			operation.EventSealedThrough = progress.EventSealedThrough
			operation.EventSealTo = progress.EventSealTo
			operation.EventWatermarkAdvanced = progress.EventWatermarkAdvanced
			operation.EventWatermark = progress.EventWatermark
		}
		if operation.Status != "completed" {
			operation.Status = "running"
		}
		if complete {
			if operation.CompletedAt.IsZero() {
				operation.Status, operation.CompletedAt = "completed", time.Now().UTC()
			}
			operation.Status = "completed"
			if operation.ExpiresAt.IsZero() {
				operation.ExpiresAt = operation.CompletedAt.Add(s.diagnosticsConfig().HistoryCleanupResultRetention)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if complete {
		return s.expireHistoryCleanupIdempotency(ctx, identity.ID)
	}
	return nil
}

func (s *Service) expireHistoryCleanupIdempotency(ctx context.Context, operationID string) error {
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(operationID))
	if err != nil {
		return err
	}
	if operation.IdempotencyID == "" || operation.ExpiresAt.IsZero() {
		return nil
	}
	key := historyCleanupIdempotencyPrefix + operation.IdempotencyID + ".json"
	idempotency, _, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if idempotency.OperationID != operationID {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, s.store, key, func(value *historyCleanupIdempotency) error {
		if value.OperationID != operationID {
			return storage.ErrCorruptObject
		}
		value.ExpiresAt = operation.ExpiresAt
		return nil
	})
}
