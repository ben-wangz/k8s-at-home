package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
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
	token := ""
	for removed < limit {
		entries, next, err := m.store.List(ctx, "tasks/state/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			task, err := m.readTask(ctx, entry.Key)
			if err != nil {
				return removed, err
			}
			if task.UpdatedAt.After(cutoff) || task.Status == Pending || task.Status == Running || task.Status == RetryWait || task.Status == RepairPending {
				continue
			}
			if err := m.store.DeleteIfMatch(ctx, entry.Key, task.version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			m.finishInFlight(ctx, &task)
			removed++
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

func (m *Manager) Retry(ctx context.Context, id string) (Task, error) {
	task, err := m.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.Status != Failed && task.Status != RetryWait {
		return Task{}, errors.New("task is not retryable")
	}
	if task.Status == Failed && (!task.Retryable || task.RootDigest == "") {
		return Task{}, errors.New("task is not retryable")
	}
	task.Status = Pending
	task.Phase = "solidification"
	task.NextAttemptAt = time.Time{}
	task.ErrorClass = ""
	task.ErrorMessage = ""
	task.CancelRequested = false
	task.Retryable = false
	task.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (m *Manager) Cancel(ctx context.Context, id string) (Task, error) {
	task, err := m.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.RootPublishing || task.RootPublished {
		return Task{}, ErrRootPublishing
	}
	switch task.Status {
	case Solidified, RepairPending:
		return Task{}, errors.New("task cannot be cancelled in its current state")
	case Running:
		task.CancelRequested = true
	default:
		task.Status = Cancelled
		task.CancelRequested = true
	}
	task.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &task); err != nil {
		return Task{}, err
	}
	if task.Status == Cancelled {
		m.finishInFlight(ctx, &task)
	}
	return task, nil
}

func (m *Manager) BeginRootPublish(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.CancelRequested {
		return ErrCancelled
	}
	if latest.RootPublished {
		*task = latest
		return nil
	}
	if latest.RootPublishing {
		*task = latest
		return nil
	}
	latest.RootPublishing = true
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) MarkRootPublished(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.CancelRequested {
		return ErrCancelled
	}
	if latest.RootPublished {
		*task = latest
		return nil
	}
	latest.RootPublishing = false
	latest.RootPublished = true
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) AbortRootPublish(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.RootPublished {
		return ErrRootPublishing
	}
	latest.RootPublishing = false
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) SaveProgress(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	task.FencingToken = currentLease.FencingToken
	task.UpdatedAt = time.Now().UTC()
	return m.save(ctx, task)
}

func (m *Manager) CheckActive(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	current, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.Status == Cancelled || current.CancelRequested {
		return ErrCancelled
	}
	if current.FencingToken > currentLease.FencingToken {
		return lease.ErrLost
	}
	return nil
}

func (m *Manager) Leases() *lease.Manager { return m.leases }
