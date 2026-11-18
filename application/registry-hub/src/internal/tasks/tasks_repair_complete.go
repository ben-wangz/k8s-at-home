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
		m.runtimeHeartbeat()
		if worker == 0 {
			_ = m.reconcileWakeRequests(ctx)
		}
		m.dispatch(ctx, worker)
		if worker == 0 && !m.recoveryPaused.Load() && m.dispatchMu.TryLock() {
			repairCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			m.runtimeDispatch(m.repairDispatchIndex(repairCtx, worker))
			cancel()
			m.dispatchMu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.admissionBlocked.Store(false)
		case <-m.dispatchWake:
		}
	}
}

func (m *Manager) dispatch(ctx context.Context, worker int) {
	if m.recoveryPaused.Load() || m.admissionBlocked.Load() || !m.dispatchMu.TryLock() {
		return
	}
	defer m.dispatchMu.Unlock()
	m.runtimeDispatch(m.dispatchPages(ctx, worker))
}

func dispatchEligible(task Task, now time.Time) bool {
	return task.Phase == "solidification" && (task.Status == Pending || task.Status == Running || task.Status == RetryWait && !task.NextAttemptAt.After(now))
}
