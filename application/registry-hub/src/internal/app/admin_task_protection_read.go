package app

import (
	"context"
	"errors"
	"math"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

const (
	adminProtectionReadTimeout      = 5 * time.Second
	adminProtectionReadMaxRetries   = 8
	adminProtectionReadInitialDelay = 25 * time.Millisecond
	adminProtectionReadMaximumDelay = 400 * time.Millisecond
)

type adminProtectionReadFailure struct {
	sessionID  string
	phase      string
	retryCount int
	reason     string
	cause      error
}

func (e *adminProtectionReadFailure) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return "protection session disappeared while reading task state"
}

func (e *adminProtectionReadFailure) Unwrap() error {
	if e.cause != nil {
		return e.cause
	}
	return storage.ErrNotFound
}

type adminProtectionReadRetry struct {
	retries int
}

func adminTaskWithoutReleasedProtection(task tasks.Task, released map[string]struct{}, activeSessions []protectionSessionStatus) (tasks.Task, error) {
	if len(released) == 0 {
		return task, nil
	}
	if len(task.SessionIDs) > 0 {
		sessionIDs := make([]string, 0, len(task.SessionIDs))
		for _, id := range task.SessionIDs {
			if _, ok := released[id]; !ok {
				sessionIDs = append(sessionIDs, id)
			}
		}
		task.SessionIDs = sessionIDs
		if len(task.SessionIDs) == 0 {
			task.SessionIDs = nil
		}
	}
	if _, ok := released[task.ProtectionSessionID]; ok {
		task.ProtectionSessionID = ""
	}
	if len(task.SessionIDs) == 0 && task.ProtectionSessionID == "" {
		if task.RecoveryReason == "" {
			task.GCProtected = false
			task.ReservedBytes = 0
		} else {
			task.GCProtected = true
		}
		return task, nil
	}
	var reserved int64
	for _, status := range activeSessions {
		session := protectionSessionRecord{
			ID: status.ID, ScopeID: status.ScopeID, ReservationClass: status.ReservationClass, ReservedBytes: status.ReservedBytes,
		}
		reservedBytes, err := taskProtectionReservationBytes(&task, status.ID, session)
		if err != nil {
			return tasks.Task{}, err
		}
		if reservedBytes > math.MaxInt64-reserved {
			return tasks.Task{}, errors.New("task protection reservation overflows")
		}
		reserved += reservedBytes
	}
	task.ReservedBytes = reserved
	task.GCProtected = true
	return task, nil
}

func (r *adminProtectionReadRetry) wait(ctx context.Context, sessionID, phase string) error {
	if r.retries >= adminProtectionReadMaxRetries {
		return &adminProtectionReadFailure{
			sessionID: sessionID, phase: phase, retryCount: r.retries,
			reason: "retry_limit", cause: storage.ErrNotFound,
		}
	}
	delay := adminProtectionReadInitialDelay << r.retries
	if delay > adminProtectionReadMaximumDelay {
		delay = adminProtectionReadMaximumDelay
	}
	r.retries++
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return adminProtectionReadError(sessionID, phase, r.retries, ctx.Err())
	case <-timer.C:
		return nil
	}
}

func adminProtectionReadError(sessionID, phase string, retries int, cause error) error {
	reason := "storage_error"
	if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled) {
		reason = "context_done"
	}
	return &adminProtectionReadFailure{
		sessionID: sessionID, phase: phase, retryCount: retries, reason: reason, cause: cause,
	}
}

func (s *Service) logAdminTaskViewFailure(taskID string, err error) {
	if s.logger == nil {
		return
	}
	var readFailure *adminProtectionReadFailure
	if errors.As(err, &readFailure) {
		s.logger.Warn("admin task protection view failed",
			"task_id", taskID, "session_id", readFailure.sessionID,
			"phase", readFailure.phase, "retry_count", readFailure.retryCount,
			"reason", readFailure.reason, "error", readFailure.cause)
		return
	}
	s.logger.Warn("admin task protection view failed", "task_id", taskID, "phase", "protection_state_read", "error", err)
}
