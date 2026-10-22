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
	var reserved int64
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
			continue
		}
		if active {
			activeIDs = append(activeIDs, id)
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
	}
	barrierErr := s.gate.SetRecoveryBarrier(ctx, reason)
	task.SessionIDs = activeIDs
	task.RequestedBytes = reserved
	task.ReservedBytes = reserved
	task.GCProtected = true
	task.RecoveryReason = reason
	if saveErr := s.tasks.SaveTerminalProtection(ctx, task, true, reason); saveErr != nil {
		barrierErr = errors.Join(barrierErr, saveErr)
	}
	return errors.Join(cause, markErr, barrierErr)
}
