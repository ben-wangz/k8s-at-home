package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) SetRecoveryReason(ctx context.Context, id, reason string) error {
	if len(reason) > 64 || !validWaitReason(reason) {
		return errors.New("task recovery reason is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.RecoveryReason == reason {
			return nil
		}
		task.RecoveryReason = reason
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (m *Manager) SetProtectedCacheBytes(ctx context.Context, id string, bytes int64) error {
	if bytes < 0 {
		return errors.New("task protected cache bytes cannot be negative")
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.Kind != BlobKind {
			return errors.New("protected cache bytes can only be recorded on a blob task")
		}
		if task.ProtectedCacheBytes == bytes {
			return nil
		}
		task.ProtectedCacheBytes = bytes
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}
