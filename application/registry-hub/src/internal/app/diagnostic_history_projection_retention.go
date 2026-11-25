package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/storage"
)

type terminalHistoryRetryKey struct{}
type terminalHistoryRetentionWorkerKey struct{}

func (s *Service) expireTerminalHistoryProjection(ctx context.Context, entry storage.Entry, prefix string, p *sourceSweepCheckpoint) error {
	if kind, _ := ctx.Value(terminalHistoryRetentionWorkerKey{}).(string); kind != "" {
		// Persist the work before deleting the object that makes it discoverable.
		return s.enqueueSourceSweepRetry(ctx, kind, entry, nil, 0, time.Now().UTC())
	}
	deleted, missing, err := s.deleteHistoryEntryConditional(ctx, entry)
	if err != nil {
		return err
	}
	if err := s.releaseHistoryProjectionBudget(ctx, prefix, entry.Key, entry.Size); err != nil {
		return err
	}
	if deleted {
		incrementSweepCount(p, "deleted", 1)
	} else if missing {
		incrementSweepCount(p, "missing", 1)
	}
	return nil
}

func (s *Service) releaseMissingTerminalHistoryBudget(ctx context.Context, kind, key string, size int64) error {
	prefix := terminalTransferHistoryPrefix
	if kind == terminalWorkHistoryRetentionWorker {
		prefix = terminalWorkHistoryPrefix
	}
	if !strings.HasPrefix(key, prefix) || size < 0 {
		return storage.ErrCorruptObject
	}
	if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return s.releaseHistoryProjectionBudget(ctx, prefix, key, size)
}
