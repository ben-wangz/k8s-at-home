package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func (s *solidifier) blobContentGeneration(ctx context.Context, task *tasks.Task, digest string) (uint64, error) {
	return s.contentGeneration(ctx, task.Authority, task.Repository, digest)
}

func (s *solidifier) contentGeneration(ctx context.Context, authority, repository, digest string) (uint64, error) {
	current, err := s.service.generation(ctx, authority, repository, digest)
	if err != nil {
		return 0, err
	}
	return current.Generation, nil
}

func (s *solidifier) releaseEmptyManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string) error {
	if err := s.gate.ReleaseEmptyProtection(ctx, sessionID, task.ID); err != nil {
		if errors.Is(err, ErrProtectionBusy) || errors.Is(err, ErrProtectionHasContent) {
			return nil
		}
		return temporary("protection_release", err)
	}
	return s.removeRootProtectionSession(ctx, task, taskLease, sessionID)
}

func (s *solidifier) removeRootProtectionSession(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string) error {
	filtered := task.SessionIDs[:0]
	for _, id := range task.SessionIDs {
		if id != sessionID {
			filtered = append(filtered, id)
		}
	}
	task.SessionIDs = filtered
	if len(task.SessionIDs) == 0 {
		task.GCProtected = false
		task.ReservedBytes = 0
	} else {
		task.GCProtected = true
		var reserved int64
		for _, id := range task.SessionIDs {
			session, active, err := s.gate.protectionSession(ctx, id)
			if err != nil {
				return temporary("protection_state", err)
			}
			if active {
				sessionBytes, err := taskProtectionReservationBytes(task, id, session)
				if err != nil {
					return temporary("protection_state", err)
				}
				if sessionBytes > math.MaxInt64-reserved {
					return temporary("protection_state", errors.New("task protection reservation overflows"))
				}
				reserved += sessionBytes
			}
		}
		task.ReservedBytes = reserved
	}
	if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func stagedSessionID(scopeID string) string {
	return hashID("scope-session", scopeID)
}

func stagedDemandID(rootTaskID, scopeID string) string {
	return hashID("scope-demand", rootTaskID, scopeID)
}

func rootTaskHasProtection(task *tasks.Task, sessionID string) bool {
	for _, id := range task.SessionIDs {
		if id == sessionID {
			return true
		}
	}
	return false
}
