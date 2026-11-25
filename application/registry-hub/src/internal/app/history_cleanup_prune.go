package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) pruneHistoryCleanupPreviews(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupPreviewPrefix, "", 1000)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		preview, version, err := inventory.Read[historyCleanupPreview](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if preview.ExpiresAt.After(now) {
			continue
		}
		if err := s.store.DeleteIfMatch(ctx, entry.Key, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) pruneHistoryCleanupOperations(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupOperationPrefix, "", 1000)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	operations := make([]historyCleanupOperation, 0, len(entries))
	for _, entry := range entries {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.SchemaVersion != 1 || operation.ID == "" {
			return storage.ErrCorruptObject
		}
		operation, err = s.refreshHistoryCleanupOperationStatus(ctx, operation)
		if err != nil {
			return err
		}
		if (operation.Status == "completed" || operation.Status == "failed") && !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(now) {
			entry, err = s.store.Head(ctx, entry.Key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := s.deleteHistoryCleanupOperation(ctx, entry, operation); err != nil {
				return err
			}
			continue
		}
		operations = append(operations, operation)
	}
	terminal := make([]historyCleanupOperation, 0, len(operations))
	for _, operation := range operations {
		if operation.Status == "completed" || operation.Status == "failed" {
			terminal = append(terminal, operation)
		}
	}
	sort.Slice(terminal, func(i, j int) bool {
		return historyCleanupTerminalTime(terminal[i]).After(historyCleanupTerminalTime(terminal[j]))
	})
	if len(terminal) > historyCleanupMaxResults {
		for _, operation := range terminal[historyCleanupMaxResults:] {
			entry, err := s.store.Head(ctx, historyCleanupOperationKey(operation.ID))
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := s.deleteHistoryCleanupOperation(ctx, entry, operation); err != nil {
				return err
			}
		}
	}
	return s.pruneHistoryCleanupIdempotency(ctx, now)
}

func historyCleanupTerminalTime(operation historyCleanupOperation) time.Time {
	if !operation.CompletedAt.IsZero() {
		return operation.CompletedAt
	}
	return operation.ExpiresAt
}

func (s *Service) deleteHistoryCleanupOperation(ctx context.Context, entry storage.Entry, operation historyCleanupOperation) error {
	if operation.Status == "failed" {
		if err := s.retireFailedHistoryCleanupJob(ctx, historyCleanupWorkKind, operation.ID); err != nil {
			return err
		}
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	if err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if operation.IdempotencyID != "" {
		key := historyCleanupIdempotencyPrefix + operation.IdempotencyID + ".json"
		return s.deleteHistoryCleanupIdempotencyForOperation(ctx, key, operation.ID)
	}
	return nil
}

func (s *Service) deleteHistoryCleanupIdempotencyForOperation(ctx context.Context, key, id string) error {
	record, version, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) || err == nil && record.OperationID != id {
		return nil
	}
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	err = s.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) pruneHistoryCleanupIdempotency(ctx context.Context, now time.Time) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupIdempotencyPrefix, "", 1000)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		idempotency, version, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !idempotency.ExpiresAt.IsZero() && !idempotency.ExpiresAt.After(now) {
			if err := s.store.DeleteIfMatch(ctx, entry.Key, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}
