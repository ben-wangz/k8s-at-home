package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) Cleanup(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if olderThan < 24*time.Hour {
		return 0, errors.New("task cleanup retention cannot be shorter than 24 hours")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	removed := 0
	recordBudget := limit
	token := ""
	for removed < limit && recordBudget > 0 {
		entries, next, err := m.store.List(ctx, "tasks/state/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if recordBudget <= 0 {
				return removed, nil
			}
			task, err := m.readTask(ctx, entry.Key)
			if err != nil {
				return removed, err
			}
			if isActive(task.Status) || task.GCProtected || len(task.SessionIDs) > 0 || task.ProtectionSessionID != "" || task.RecoveryReason != "" {
				continue
			}
			if !task.CleanupStarted && task.UpdatedAt.After(cutoff) {
				continue
			}
			records := NewRecords(m.store)
			if task.Kind == BlobKind {
				active, err := m.hasActiveBlobDemand(ctx, records, task.ID)
				if err != nil {
					return removed, err
				}
				if active {
					continue
				}
			}
			if !task.CleanupStarted {
				task.CleanupStarted = true
				if err := m.save(ctx, &task); err != nil {
					if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
						continue
					}
					return removed, err
				}
			}
			if keeper, ok := m.processor.(interface {
				PreserveTaskHistoryStep(context.Context, *Task) (bool, error)
			}); ok {
				done := false
				stepLimit := min(recordBudget, 500)
				for step := 0; step < stepLimit; step++ {
					var err error
					done, err = keeper.PreserveTaskHistoryStep(ctx, &task)
					if err != nil {
						return removed, err
					}
					if done {
						break
					}
					recordBudget--
				}
				if !done {
					continue
				}
			} else if keeper, ok := m.processor.(interface {
				PreserveTaskHistory(context.Context, *Task) error
			}); ok {
				if err := keeper.PreserveTaskHistory(ctx, &task); err != nil {
					return removed, err
				}
			}
			if recordBudget == 0 {
				return removed, nil
			}
			batchLimit := min(recordBudget, 500)
			if task.Kind == RootKind {
				n, done, err := records.DeleteTaskGraph(ctx, task.ID, batchLimit)
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
				n, done, err = records.DeleteBlobDemandsForRoot(ctx, task.ID, min(recordBudget, batchLimit))
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
			} else if task.Kind == BlobKind {
				n, done, err := records.DeleteBlobIntentHistory(ctx, task.ID, batchLimit)
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
			}
			n, done, err := records.DeleteCacheProtectionReferences(ctx, task.ID, min(recordBudget, batchLimit))
			recordBudget -= n
			if err != nil {
				return removed, err
			}
			if !done || recordBudget == 0 {
				continue
			}
			latest, err := m.Get(ctx, task.ID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if !latest.CleanupStarted || isActive(latest.Status) || latest.GCProtected || len(latest.SessionIDs) > 0 || latest.ProtectionSessionID != "" || latest.RecoveryReason != "" {
				continue
			}
			if err := m.clearWakeRequest(ctx, task.ID); err != nil {
				return removed, err
			}
			if err := m.store.DeleteIfMatch(ctx, entry.Key, latest.version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			m.finishInFlight(ctx, &task)
			removed++
			recordBudget--
			if removed >= limit {
				return removed, nil
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}

func (m *Manager) hasActiveBlobDemand(ctx context.Context, records *Records, intentID string) (bool, error) {
	token := ""
	for {
		demands, next, err := records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		for _, demand := range demands {
			if demand.RootTaskID == "" {
				continue
			}
			root, err := m.Get(ctx, demand.RootTaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if isActive(root.Status) || root.GCProtected || len(root.SessionIDs) > 0 || root.ProtectionSessionID != "" || root.RecoveryReason != "" {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}
