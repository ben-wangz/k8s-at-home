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
			latest.CancelPhase = "quiesced"
			latest.UpdatedAt = time.Now().UTC()
			if m.save(parent, &latest) == nil {
				m.finishInFlight(parent, &latest)
				m.notifyTerminal(parent, &latest)
				_ = m.clearWakeRequest(parent, latest.ID)
			}
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
	m.mu.Lock()
	if m.cancels == nil {
		m.cancels = make(map[string]context.CancelFunc)
	}
	m.cancels[latest.ID] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, latest.ID)
		m.mu.Unlock()
	}()
	if current, err := m.Get(parent, latest.ID); err == nil && current.CancelRequested {
		cancel()
	}
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
	if currentErr == nil {
		latest.RootPublishing = current.RootPublishing
		latest.RootPublished = current.RootPublished
		if current.CancelRequested {
			latest.CancelRequested = true
			latest.CancelPhase = current.CancelPhase
		}
		latest.version = current.version
	}
	if latest.CancelRequested || errors.Is(err, ErrCancelled) {
		latest.Status = Cancelled
		latest.CancelPhase = "quiesced"
		latest.WaitReason = ""
		clearTaskQuotaWait(&latest)
		latest.Retryable = false
		latest.NextAttemptAt = time.Time{}
	} else if err != nil {
		var yielded *Yield
		if errors.As(err, &yielded) && isWaiting(yielded.Status) && validWaitReason(yielded.Reason) {
			latest.Status = yielded.Status
			latest.WaitReason = yielded.Reason
			if yielded.Status == WaitingQuota {
				if latest.QuotaWaitAt.IsZero() || latest.QuotaWaitReason != yielded.Reason {
					latest.QuotaWaitAt = time.Now().UTC()
					latest.QuotaWaitReason = yielded.Reason
				}
			} else {
				clearTaskQuotaWait(&latest)
			}
			latest.Retryable = false
			latest.NextAttemptAt = time.Time{}
			latest.ErrorClass = ""
			latest.ErrorMessage = ""
		} else {
			clearTaskQuotaWait(&latest)
			m.finishFailure(&latest, err)
		}
	} else {
		latest.Status = Solidified
		latest.WaitReason = ""
		clearTaskQuotaWait(&latest)
		latest.LastProgressAt = time.Now().UTC()
		latest.ExpiresAt = latest.LastProgressAt.Add(m.idleTimeout)
		latest.Retryable = false
		latest.NextAttemptAt = time.Time{}
		latest.ErrorClass = ""
		latest.ErrorMessage = ""
	}
	latest.UpdatedAt = time.Now().UTC()
	if isWaiting(latest.Status) {
		if m.leases.Validate(parent, currentLease) == nil {
			if err := m.save(parent, &latest); err == nil {
				m.notifyYielded(parent, &latest)
			}
		}
		return
	}
	if m.leases.Validate(parent, currentLease) == nil {
		if saveErr := m.save(parent, &latest); saveErr == nil {
			if latest.Status == Solidified || latest.Status == Failed || latest.Status == Cancelled {
				m.finishInFlight(parent, &latest)
				m.notifyTerminal(parent, &latest)
			}
			if !isWaiting(latest.Status) {
				_ = m.clearWakeRequest(parent, latest.ID)
			}
		}
	}
}

func clearTaskQuotaWait(task *Task) {
	task.QuotaWaitAt = time.Time{}
	task.QuotaWaitReason = ""
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
}

func (m *Manager) notifyYielded(ctx context.Context, task *Task) {
	handler, ok := m.processor.(TaskYieldedHandler)
	if ok {
		handler.TaskYielded(ctx, task)
	}
}

func (m *Manager) notifyTerminal(ctx context.Context, task *Task) {
	handler, ok := m.processor.(TaskTerminalHandler)
	if !ok {
		return
	}
	_ = handler.TaskTerminal(ctx, task)
}

func (m *Manager) finishFailure(task *Task, err error) {
	var failure *Failure
	if errors.As(err, &failure) && failure.RepairPending {
		task.Status = RepairPending
		task.Retryable = false
		task.NextAttemptAt = time.Time{}
	} else if errors.As(err, &failure) && failure.Permanent {
		task.Status = Failed
		task.Retryable = false
		task.NextAttemptAt = time.Time{}
	} else {
		task.Status = RetryWait
		task.Retryable = true
		task.RetryCount++
		task.NextAttemptAt = time.Now().UTC().Add(retryDelay(task.RetryCount))
	}
	task.WaitReason = ""
	if errors.As(err, &failure) {
		task.ErrorClass = failure.Class
		task.ErrorMessage = safeMessage(failure.Err)
	} else {
		task.ErrorClass = "upstream_or_storage"
		task.ErrorMessage = safeMessage(err)
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
