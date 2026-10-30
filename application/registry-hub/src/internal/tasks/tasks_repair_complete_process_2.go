package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/diagnostics"
	"strconv"
	"time"
)

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
	latest.ParentAttemptID = latest.AttemptID
	latest.AttemptID = diagnostics.ID("solidification_attempt", latest.ID, strconv.FormatUint(currentLease.FencingToken, 10))
	latest.AttemptStartedAt, latest.AttemptFinishedAt = time.Now().UTC(), time.Time{}
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
		if current.ManifestObservedAt.After(latest.ManifestObservedAt) {
			latest.ManifestObservedAt, latest.ManifestGeneration = current.ManifestObservedAt, current.ManifestGeneration
		}
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
		latest.AttemptFinishedAt = latest.UpdatedAt
		if m.leases.Validate(parent, currentLease) == nil {
			if err := m.save(parent, &latest); err == nil {
				m.notifyYielded(parent, &latest)
			}
		}
		return
	}
	if m.leases.Validate(parent, currentLease) == nil {
		latest.AttemptFinishedAt = latest.UpdatedAt
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
