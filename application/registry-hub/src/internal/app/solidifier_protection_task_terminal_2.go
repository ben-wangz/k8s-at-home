package app

import (
	"context"
	"errors"
	"registry-hub/internal/tasks"
)

func (s *solidifier) TaskTerminal(ctx context.Context, task *tasks.Task) error {
	if hasLegacyRecoveryBlocker(*task) && !hasTaskProtectionReferences(*task) && !safeLegacyTerminalTask(*task) {
		return errors.New("legacy task recovery blocker cannot be cleared from terminal task state")
	}
	if task.Status != tasks.Failed && task.Status != tasks.Cancelled && task.Status != tasks.Solidified {
		return nil
	}
	if task.Kind == tasks.RootKind {
		if reason, err := s.revokeRootBlobDemands(ctx, task.ID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, reason, err)
		}
	}
	if task.Kind == tasks.BlobKind {
		switch task.Status {
		case tasks.Failed:
			_, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentFailed, task.ErrorClass, task.ErrorMessage)
			if err != nil {
				return err
			}
		case tasks.Solidified:
			if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentAnchored, "", ""); err != nil {
				return err
			}
		}
	}
	if task.Kind == tasks.BlobKind && task.Status == tasks.Cancelled {
		if err := s.service.records.RemoveBlobDemand(ctx, tasks.BlobDemand{IntentID: task.ID, DemandID: task.ID}); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, "demand_remove_unknown", err)
		}
	}
	if task.Kind == tasks.BlobKind {
		if err := s.releaseProtectedCacheReservation(ctx, task); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, "cache_reservation_release_unknown", err)
		}
	}
	sessions, err := s.protectionSessionsOwnedBy(ctx, task.ID)
	if err != nil {
		return s.requireTaskProtectionRecovery(ctx, task, nil, "owner_lookup_unknown", err)
	}
	sessions = uniqueSessionIDs(append(sessions, task.SessionIDs...))
	for _, sessionID := range sessions {
		if sessionID == legacyGrandfatheredSessionID {
			session, active, err := s.gate.protectionSession(ctx, sessionID)
			if err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
			}
			if !active {
				continue
			}
			if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_identity_unknown", errors.New("legacy grandfathered session identity is invalid"))
			}
			remaining, err := s.gate.RemoveProtectionOwner(ctx, sessionID, task.ID)
			if err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "owner_removal_unknown", err)
			}
			if remaining == 0 {
				if err := s.releaseLegacyGrandfatheredSessionIfUnowned(ctx); err != nil {
					return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "legacy_release_unknown", err)
				}
			}
			continue
		}
		if task.Status == tasks.Solidified {
			if err := s.releaseAnchoredSession(ctx, sessionID); err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "anchor_release_unknown", err)
			}
			continue
		}
		remaining, err := s.gate.RemoveProtectionOwner(ctx, sessionID, task.ID)
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "owner_removal_unknown", err)
		}
		if remaining > 0 {
			continue
		}
		session, active, err := s.gate.protectionSession(ctx, sessionID)
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
		}
		if !active {
			continue
		}
		if session.State == "anchored" {
			if err := s.gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "anchor_release_unknown", err)
			}
			continue
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "remote_result_unknown", errors.New("terminal task still has an unresolved remote operation"))
		}
		stop, err := s.gate.RequestProtectionStop(ctx, sessionID, terminalProtectionReason(task.Status))
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "stop_request_unknown", err)
		}
		if !stop {
			continue
		}
		if err := s.gate.MarkProtectionQuiescing(ctx, sessionID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "quiescence_unknown", err)
		}
		session, active, err = s.gate.protectionSession(ctx, sessionID)
		if err != nil || !active {
			if err == nil {
				continue
			}
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "remote_result_unknown", errors.New("terminal task still has an unresolved remote operation"))
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, sessionID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "quiescence_unknown", err)
		}
		if err := s.gate.ReleaseProtection(ctx, sessionID, terminalProtectionReason(task.Status)); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "release_unknown", err)
		}
	}
	if task.Kind == tasks.BlobKind && task.Status == tasks.Cancelled {
		if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentCancelled, "", ""); err != nil {
			return err
		}
	}
	s.wakeOldestQuotaTask(ctx)
	if task.Kind == tasks.BlobKind {
		s.service.wakeBlobDemandRoots(ctx, task.ID)
	}
	s.clearTaskProtection(task)
	return s.tasks.SaveTerminalProtection(ctx, task, false, "")
}

func (s *solidifier) revokeRootBlobDemands(ctx context.Context, rootTaskID string) (string, error) {
	token := ""
	for {
		demands, next, err := s.service.records.ListRootBlobDemandRecords(ctx, rootTaskID, token, 500)
		if err != nil {
			return "demand_list_unknown", err
		}
		for _, demand := range demands {
			if err := s.service.records.RemoveBlobDemand(ctx, demand); err != nil {
				return "demand_remove_unknown", err
			}
			if reason, err := s.cancelBlobIntentWithoutDemand(ctx, demand.IntentID); err != nil {
				if s.service.logger != nil {
					s.service.logger.Warn("root blob demand cancellation failed", "root_task_id", rootTaskID, "intent_id", demand.IntentID, "recovery_reason", reason, "error", err)
				}
				return reason, err
			}
			if err := s.service.records.DeleteRootBlobDemandRecord(ctx, demand); err != nil {
				return "demand_index_delete_unknown", err
			}
		}
		if next == "" {
			return "", nil
		}
		token = next
	}
}
