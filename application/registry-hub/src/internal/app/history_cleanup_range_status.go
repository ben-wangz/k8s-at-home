package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

func (s *Service) checkHistoryCleanupRangeCapacity(ctx context.Context) error {
	activeCount := 0
	var activeOperations map[string]bool
	now := time.Now().UTC()
	return s.scanHistoryCleanupRecords(ctx, historyCleanupRangePrefix, func(entry storage.Entry) error {
		if strings.HasSuffix(entry.Key, ".progress.json") {
			return nil
		}
		preview, _, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		inactive := preview.Status == "failed" || !preview.ExpiresAt.After(now)
		if !inactive && preview.Status != "completed" {
			inactive, err = s.historyCleanupPreviewFailed(ctx, preview.ID)
			if err != nil {
				return err
			}
		}
		if inactive {
			if activeOperations == nil {
				activeOperations, err = s.activeHistoryCleanupPreviews(ctx)
				if err != nil {
					return err
				}
			}
			if !activeOperations[preview.ID] {
				return nil
			}
		}
		activeCount++
		if activeCount >= historyCleanupMaxPreviews {
			return errHistoryCleanupCapacity
		}
		return nil
	})
}

func (s *Service) historyCleanupRangeHasActiveOperation(ctx context.Context, previewID string) (bool, error) {
	active, err := s.activeHistoryCleanupPreviews(ctx)
	return active[previewID], err
}

func (s *Service) activeHistoryCleanupPreviews(ctx context.Context) (map[string]bool, error) {
	active := map[string]bool{}
	err := s.scanHistoryCleanupRecords(ctx, historyCleanupOperationPrefix, func(entry storage.Entry) error {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if operation.Status == "completed" || operation.Status == "failed" {
			return nil
		}
		job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, operation.ID)
		if errors.Is(err, storage.ErrNotFound) || err == nil && job.State != "failed" && job.State != "done" {
			active[operation.PreviewID] = true
			return nil
		}
		return err
	})
	return active, err
}

func (s *Service) scanHistoryCleanupRecords(ctx context.Context, prefix string, visit func(storage.Entry) error) error {
	token := ""
	for {
		entries, next, err := storage.Scan(ctx, s.store, prefix, token, historyCleanupBatchSize)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := visit(entry); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		if token == next {
			return storage.ErrUnavailable
		}
		token = next
	}
}

func (s *Service) historyCleanupPreviewFailed(ctx context.Context, previewID string) (bool, error) {
	job, err := s.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, previewID))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return job.State == "failed", nil
}

func (s *Service) historyCleanupRangePreviewStatus(ctx context.Context, id, principal string) (historyCleanupRangePreview, error) {
	preview, _, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, historyCleanupRangePreviewKey(id))
	if errors.Is(err, storage.ErrNotFound) || err == nil && preview.Principal != principal {
		return historyCleanupRangePreview{}, storage.ErrNotFound
	}
	if err != nil {
		return historyCleanupRangePreview{}, err
	}
	if !preview.ExpiresAt.After(time.Now().UTC()) {
		return historyCleanupRangePreview{}, inventory.ErrExpired
	}
	job, jobErr := s.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, id))
	if jobErr == nil {
		if job.State == "retry_wait" || job.State == "failed" {
			preview.Status, preview.LastError = job.State, job.ErrorClass
			preview.NextRetry = job.NextAttemptAt
		} else if job.State == "done" {
			preview.Status = "completed"
		}
	} else if !errors.Is(jobErr, storage.ErrNotFound) {
		return historyCleanupRangePreview{}, jobErr
	} else if preview.Status != "completed" {
		if err := s.enqueueHistoryCleanupRangePreview(ctx, id, principal); err != nil {
			return historyCleanupRangePreview{}, err
		}
	}
	return preview, nil
}

func (s *Service) historyCleanupRangeBackfillPending(ctx context.Context, category string) ([]string, error) {
	worker := ""
	switch category {
	case "terminal_work_history":
		worker = terminalWorkHistoryRetentionWorker
	case "terminal_transfer_history":
		worker = terminalTransferHistoryRetentionWorker
	default:
		return nil, nil
	}
	checkpoint, _, err := inventory.Read[sourceSweepCheckpoint](ctx, s.store, sourceSweepKey(worker))
	if errors.Is(err, storage.ErrNotFound) {
		return []string{worker}, nil
	}
	if err != nil {
		return nil, err
	}
	if checkpoint.HistoryIndexBackfillVersion != terminalHistoryIndexBackfillVersion || !checkpoint.HistoryIndexBackfillComplete {
		return []string{worker}, nil
	}
	return nil, nil
}
