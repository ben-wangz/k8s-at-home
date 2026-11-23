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
		operation, version, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.SchemaVersion != 1 || operation.ID == "" {
			return storage.ErrCorruptObject
		}
		if operation.Status == "completed" && !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(now) {
			entry.Version = version
			if err := s.deleteHistoryCleanupOperation(ctx, entry, operation); err != nil {
				return err
			}
			continue
		}
		operations = append(operations, operation)
	}
	terminal := make([]historyCleanupOperation, 0, len(operations))
	for _, operation := range operations {
		if operation.Status == "completed" {
			terminal = append(terminal, operation)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].CompletedAt.After(terminal[j].CompletedAt) })
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

func (s *Service) deleteHistoryCleanupOperation(ctx context.Context, entry storage.Entry, operation historyCleanupOperation) error {
	if err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if operation.IdempotencyID != "" {
		if idempotency, err := s.store.Head(ctx, historyCleanupIdempotencyPrefix+operation.IdempotencyID+".json"); err == nil {
			if err := s.store.DeleteIfMatch(ctx, idempotency.Key, idempotency.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
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
