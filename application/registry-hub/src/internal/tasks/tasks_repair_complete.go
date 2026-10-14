package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) RepairComplete(ctx context.Context, id string) error {
	task, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if task.Status != RepairPending {
		return nil
	}
	currentLease, err := m.leases.Acquire(ctx, "tasks/"+task.ID, m.owner+"-repair", 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = m.leases.Release(context.Background(), currentLease) }()
	latest, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if latest.Status != RepairPending {
		if latest.Status == Pending || latest.Status == Running || latest.Status == RetryWait {
			return storage.ErrConditionFailed
		}
		return nil
	}
	latest.Status = Solidified
	if !latest.RootPublished {
		latest.Status = Pending
		latest.Phase = "solidification"
	}
	latest.RootPublishing = false
	latest.Retryable = false
	latest.ErrorClass = ""
	latest.ErrorMessage = ""
	latest.NextAttemptAt = time.Time{}
	latest.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &latest); err != nil {
		return err
	}
	if latest.Status == Solidified {
		m.finishInFlight(ctx, &latest)
	}
	return nil
}

func (m *Manager) RepairSuperseded(ctx context.Context, id string) error {
	task, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if task.Status != RepairPending {
		if task.Status == Pending || task.Status == Running || task.Status == RetryWait {
			return storage.ErrConditionFailed
		}
		return nil
	}
	currentLease, err := m.leases.Acquire(ctx, "tasks/"+task.ID, m.owner+"-repair", 30*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = m.leases.Release(context.Background(), currentLease) }()
	latest, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if latest.Status != RepairPending {
		if latest.Status == Pending || latest.Status == Running || latest.Status == RetryWait {
			return storage.ErrConditionFailed
		}
		return nil
	}
	latest.Status = Failed
	latest.RootPublishing = false
	latest.Retryable = false
	latest.ErrorClass = "repair_superseded"
	latest.ErrorMessage = "referrer repair was superseded by a newer operation"
	latest.NextAttemptAt = time.Time{}
	latest.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &latest); err != nil {
		return err
	}
	m.finishInFlight(ctx, &latest)
	return nil
}

func (m *Manager) run(ctx context.Context, worker int) {
	defer m.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		m.dispatch(ctx, worker)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) dispatch(ctx context.Context, worker int) {
	tasks, err := m.listActive(ctx)
	if err != nil {
		return
	}
	for _, task := range tasks {
		if ctx.Err() != nil {
			return
		}
		if task.Phase != "solidification" || task.Status != Pending && task.Status != RetryWait && task.Status != Running {
			continue
		}
		if task.Status == RetryWait && task.NextAttemptAt.After(time.Now()) {
			continue
		}
		m.mu.Lock()
		if _, running := m.active[task.ID]; running || len(m.active) >= m.workers {
			m.mu.Unlock()
			continue
		}
		m.active[task.ID] = struct{}{}
		m.mu.Unlock()
		m.wg.Add(1)
		go func(item Task) {
			defer m.wg.Done()
			defer func() {
				m.mu.Lock()
				delete(m.active, item.ID)
				m.mu.Unlock()
			}()
			m.process(ctx, worker, item)
		}(task)
	}
}

func (m *Manager) process(parent context.Context, worker int, task Task) {
	owner := fmt.Sprintf("%s-%d", m.owner, worker)
	currentLease, err := m.leases.Acquire(parent, "tasks/"+task.ID, owner, 60*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = m.leases.Release(context.Background(), currentLease) }()
	latest, err := m.Get(parent, task.ID)
	if err != nil || latest.CancelRequested {
		if err == nil {
			latest.Status = Cancelled
			latest.UpdatedAt = time.Now().UTC()
			_ = m.save(parent, &latest)
			m.finishInFlight(parent, &latest)
		}
		return
	}
	latest.Status = Running
	latest.FencingToken = currentLease.FencingToken
	latest.UpdatedAt = time.Now().UTC()
	if err := m.save(parent, &latest); err != nil {
		return
	}
	workCtx, cancel := context.WithCancel(parent)
	defer cancel()
	renewDone := make(chan error, 1)
	go m.renewLease(workCtx, cancel, currentLease, renewDone)
	err = m.processor.Process(workCtx, &latest, currentLease)
	cancel()
	renewErr := <-renewDone
	if parent.Err() != nil {
		return
	}
	if renewErr != nil {
		return
	}
	current, currentErr := m.Get(parent, latest.ID)
	if currentErr == nil && current.CancelRequested {
		latest.CancelRequested = true
	}
	if latest.CancelRequested || errors.Is(err, ErrCancelled) {
		latest.Status = Cancelled
		latest.Retryable = false
		latest.NextAttemptAt = time.Time{}
		m.finishInFlight(parent, &latest)
	} else if err == nil {
		latest.Status = Solidified
		latest.Retryable = false
		latest.NextAttemptAt = time.Time{}
		latest.ErrorClass = ""
		latest.ErrorMessage = ""
		m.finishInFlight(parent, &latest)
	} else {
		var failure *Failure
		if errors.As(err, &failure) && failure.RepairPending {
			latest.Status = RepairPending
			latest.Retryable = false
			latest.NextAttemptAt = time.Time{}
		} else if errors.As(err, &failure) && failure.Permanent {
			latest.Status = Failed
			latest.Retryable = false
			latest.NextAttemptAt = time.Time{}
		} else {
			latest.Status = RetryWait
			latest.Retryable = true
			latest.RetryCount++
			latest.NextAttemptAt = time.Now().UTC().Add(retryDelay(latest.RetryCount))
		}
		if errors.As(err, &failure) {
			latest.ErrorClass = failure.Class
			latest.ErrorMessage = safeMessage(failure.Err)
		} else {
			latest.ErrorClass = "upstream_or_storage"
			latest.ErrorMessage = safeMessage(err)
		}
	}
	latest.UpdatedAt = time.Now().UTC()
	if saveErr := m.SaveProgress(parent, &latest, currentLease); saveErr == nil && (latest.Status == Solidified || latest.Status == Failed || latest.Status == Cancelled) {
		m.finishInFlight(parent, &latest)
	}
}

func (m *Manager) renewLease(ctx context.Context, cancel context.CancelFunc, current lease.Lease, done chan<- error) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewed, err := m.leases.Renew(ctx, current, 60*time.Second)
			if err != nil {
				cancel()
				done <- err
				return
			}
			current = renewed
		}
	}
}
