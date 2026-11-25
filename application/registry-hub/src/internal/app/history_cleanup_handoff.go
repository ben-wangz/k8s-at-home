package app

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

func (s *Service) reconcileHistoryCleanupOperation(ctx context.Context, operation historyCleanupOperation) error {
	job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, operation.ID)
	if err == nil {
		_, err := s.refreshHistoryCleanupOperationFromJob(ctx, operation, job)
		return err
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return s.recoverHistoryCleanupHandoff(ctx, operation.ID)
}

func (s *Service) recoverHistoryCleanupHandoff(ctx context.Context, id string) error {
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "diagnostics/history-cleanup-api", requestID(), 30*time.Second)
	if err != nil {
		return err
	}
	defer manager.Release(ctx, holder)
	guarded := inventory.WithWriteGuard(ctx, func(check context.Context) error {
		return manager.Validate(check, holder)
	})
	key := historyCleanupOperationKey(id)
	operation, version, err := inventory.Read[historyCleanupOperation](guarded, s.store, key)
	if err != nil || operation.Status == "completed" || operation.Status == "failed" {
		return err
	}
	if _, err := s.backgroundQueue().Read(guarded, historyCleanupWorkKind, id); !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	committed, err := s.historyCleanupSubmissionCommitted(guarded, operation)
	if err != nil {
		return err
	}
	if committed {
		if operation.Status != "pending" && operation.Status != "running" {
			return storage.ErrCorruptObject
		}
		return s.enqueueHistoryCleanupOperation(guarded, operation)
	}
	if operation.CreatedAt.IsZero() || time.Since(operation.CreatedAt) < s.diagnosticsConfig().HistoryCleanupPreviewTTL {
		return nil
	}
	if err := inventory.ValidateWriteGuard(guarded); err != nil {
		return err
	}
	return s.store.DeleteIfMatch(guarded, key, version)
}

func (s *Service) historyCleanupSubmissionCommitted(ctx context.Context, operation historyCleanupOperation) (bool, error) {
	if operation.IdempotencyID == "" {
		return false, nil
	}
	key := historyCleanupIdempotencyPrefix + operation.IdempotencyID + ".json"
	record, _, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return record.SchemaVersion == 1 && record.OperationID == operation.ID &&
		record.RequestID == historyCleanupRequestHash(operation.PreviewID, operation.SegmentID) &&
		(record.ExpiresAt.IsZero() || record.ExpiresAt.After(time.Now().UTC())), nil
}
