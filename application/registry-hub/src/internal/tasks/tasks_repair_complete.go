package tasks

import (
	"context"
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
		m.notifyTerminal(ctx, &latest)
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
		if worker == 0 {
			_ = m.reconcileWakeRequests(ctx)
		}
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
