package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) requireTaskProtectionRecovery(ctx context.Context, task *tasks.Task, sessionIDs []string, reason string, cause error) error {
	ids := append([]string(nil), sessionIDs...)
	ids = append(ids, task.SessionIDs...)
	if task.ProtectionSessionID != "" {
		ids = append(ids, task.ProtectionSessionID)
	}
	unique := make(map[string]bool, len(ids))
	var markErr error
	activeIDs := make([]string, 0, len(ids))
	retainedIDs := make([]string, 0, len(ids))
	var reserved int64
	activeSessionReadUnknown := false
	for _, id := range ids {
		if id == "" || unique[id] {
			continue
		}
		unique[id] = true
		if err := s.gate.MarkProtectionRecoveryRequired(ctx, id, reason); err != nil && !errors.Is(err, storage.ErrNotFound) {
			markErr = errors.Join(markErr, err)
		}
		session, active, err := s.gate.protectionSession(ctx, id)
		if err != nil {
			markErr = errors.Join(markErr, err)
			retainedIDs = append(retainedIDs, id)
			if containsString(s.gate.State().SessionIDs, id) {
				activeSessionReadUnknown = true
			}
			continue
		}
		if !active {
			if err := s.gate.confirmInactiveProtectionSessionReleased(ctx, id, "", ""); err != nil {
				markErr = errors.Join(markErr, err)
				retainedIDs = append(retainedIDs, id)
			}
			continue
		}
		activeIDs = append(activeIDs, id)
		retainedIDs = append(retainedIDs, id)
		if task.ScopeID == "" {
			if id == legacyGrandfatheredSessionID && isLegacySolidificationTask(*task) {
				task.ScopeID, err = protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
				if err != nil {
					markErr = errors.Join(markErr, err)
				}
			} else {
				task.ScopeID = session.ScopeID
			}
		}
		sessionBytes, reserveErr := taskProtectionReservationBytes(task, id, session)
		if reserveErr != nil {
			markErr = errors.Join(markErr, reserveErr)
			sessionBytes = session.ReservedBytes
		}
		if sessionBytes > math.MaxInt64-reserved {
			markErr = errors.Join(markErr, errors.New("task protection reservation overflows"))
			reserved = math.MaxInt64
		} else {
			reserved += sessionBytes
		}
	}
	if activeSessionReadUnknown && task.ReservedBytes > reserved {
		reserved = task.ReservedBytes
	}
	barrierErr := s.gate.SetRecoveryBarrier(ctx, reason)
	task.SessionIDs = retainedIDs
	task.RequestedBytes = reserved
	task.ReservedBytes = reserved
	task.GCProtected = len(activeIDs) > 0
	task.RecoveryReason = reason
	if saveErr := s.tasks.SaveTerminalProtection(ctx, task, task.GCProtected, reason); saveErr != nil {
		barrierErr = errors.Join(barrierErr, saveErr)
	}
	recoveryErr := errors.Join(cause, markErr, barrierErr)
	if s.service.logger != nil {
		s.service.logger.Warn("terminal protection cleanup requires recovery",
			"task_id", task.ID,
			"session_ids", retainedIDs,
			"recovery_reason", reason,
			"error", recoveryErr,
		)
	}
	return recoveryErr
}
