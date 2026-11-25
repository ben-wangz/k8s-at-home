package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) deleteHistoryEntryConditional(ctx context.Context, entry storage.Entry) (bool, bool, error) {
	if entry.Version == "" {
		current, err := s.store.Head(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		if entry.ModTime != current.ModTime || entry.Size != current.Size {
			return false, false, storage.ErrConditionFailed
		}
		entry = current
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return false, false, err
	}
	err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return false, true, nil
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, headErr := s.store.Head(ctx, entry.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return true, false, nil
		}
		if headErr != nil {
			return false, false, errors.Join(err, headErr)
		}
		if sourceEntryVersion(current) != sourceEntryVersion(entry) {
			return false, false, storage.ErrConditionFailed
		}
	}
	return err == nil, false, err
}

func (s *Service) deleteStorageRetentionEntry(ctx context.Context, entry storage.Entry) error {
	err := s.deleteDiagnosticObservation(ctx, entry)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
