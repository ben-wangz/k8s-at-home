package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func (s *Service) pruneHistoryCleanupRangeEntry(ctx context.Context, entry storage.Entry) error {
	if strings.HasSuffix(entry.Key, ".progress.json") {
		if entry.ModTime == 0 || time.Since(time.Unix(0, entry.ModTime)) < time.Hour {
			return nil
		}
		previewKey := strings.TrimSuffix(entry.Key, ".progress.json") + ".json"
		if _, err := s.store.Head(ctx, previewKey); !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		_, _, err := s.deleteHistoryEntryConditional(ctx, entry)
		return err
	}
	preview, version, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, entry.Key)
	if err != nil || preview.ExpiresAt.After(time.Now().UTC()) {
		return err
	}
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "diagnostics/history-cleanup-api", requestID(), 30*time.Second)
	if err != nil {
		return err
	}
	defer manager.Release(ctx, holder)
	if active, err := s.historyCleanupRangeHasActiveOperation(ctx, preview.ID); active || err != nil {
		return err
	}
	current, err := s.store.Head(ctx, entry.Key)
	if err != nil {
		return err
	}
	if current.Version != version {
		return storage.ErrConditionFailed
	}
	progress, err := s.readHistoryCleanupRangeProgress(ctx, preview.ID, nil)
	if err != nil {
		return err
	}
	segmentID := preview.SegmentID
	if segmentID == "" {
		segmentID = progress.SegmentID
	}
	if segmentID != "" {
		if err := s.pruneHistoryCleanupSegment(ctx, segmentID); err != nil {
			return err
		}
	}
	if err := s.retireFailedHistoryCleanupJob(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID)); err != nil {
		return err
	}
	if err := s.store.Delete(ctx, historyCleanupRangeProgressKey(preview.ID)); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return s.store.DeleteIfMatch(ctx, entry.Key, version)
}

func (s *Service) pruneHistoryCleanupSegment(ctx context.Context, segmentID string) error {
	entries, next, err := storage.Scan(ctx, s.store, historyCleanupSegmentPrefix+segmentID+"/chunks/", "", 8)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		_, _, err := s.deleteHistoryEntryConditional(ctx, entry)
		if err != nil {
			return err
		}
	}
	if next != "" {
		return errSourceBudgetYield
	}
	err = s.store.Delete(ctx, historyCleanupSegmentKey(segmentID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) pruneHistoryCleanupContinuationEntry(ctx context.Context, entry storage.Entry) error {
	continuation, version, err := inventory.Read[historyCleanupRangeContinuation](ctx, s.store, entry.Key)
	if err != nil || continuation.ExpiresAt.After(time.Now().UTC()) {
		return err
	}
	return s.store.DeleteIfMatch(ctx, entry.Key, version)
}
