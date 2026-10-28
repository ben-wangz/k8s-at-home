package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) releaseLegacyGrandfatheredSessionIfUnowned(ctx context.Context) error {
	session, active, err := s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil || !active {
		return err
	}
	if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID {
		return errors.New("legacy grandfathered session identity is invalid")
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	stop, err := s.gate.RequestProtectionStop(ctx, legacyGrandfatheredSessionID, "cancelled")
	if err != nil || !stop {
		return err
	}
	session, active, err = s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	if session.State == "anchored" {
		return s.gate.ReleaseProtection(ctx, legacyGrandfatheredSessionID, "anchored")
	}
	if session.State != "quiesced" {
		if session.State != "quiescing" {
			if err := s.gate.MarkProtectionQuiescing(ctx, legacyGrandfatheredSessionID); err != nil {
				return err
			}
		}
		session, active, err = s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
		if err != nil || !active {
			return err
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return ErrProtectionBusy
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, legacyGrandfatheredSessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, legacyGrandfatheredSessionID, "cancelled")
}

func terminalProtectionReason(status tasks.State) string {
	if status == tasks.Failed {
		return "failed"
	}
	return "cancelled"
}

func (s *solidifier) protectionSessionsOwnedBy(ctx context.Context, ownerID string) ([]string, error) {
	var result []string
	token := ""
	for {
		sessions, next, err := s.gate.ListProtectionSessions(ctx, token, 500)
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			owners, err := s.gate.ProtectionSessionOwners(ctx, session.ID)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return nil, err
			}
			for _, owner := range owners {
				if owner == ownerID {
					result = append(result, session.ID)
					break
				}
			}
		}
		if next == "" {
			return result, nil
		}
		token = next
	}
}

func (s *solidifier) releaseAnchoredSession(ctx context.Context, sessionID string) error {
	session, active, err := s.gate.protectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if !active {
		return s.gate.confirmInactiveProtectionSessionReleased(ctx, sessionID, "", "")
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	if session.State != "anchored" {
		if err := s.gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, sessionID, "anchored")
}

func (s *solidifier) wakeOldestQuotaTask(ctx context.Context) {
	if s.service == nil {
		return
	}
	if err := s.service.wakeAvailableQuotaWaiter(ctx, "distribution_quota", s.gate.State()); err != nil && s.service.logger != nil {
		s.service.logger.Warn("quota waiter wake failed", "error_class", "storage")
	}
}

func (s *solidifier) releaseProtectedCacheReservation(ctx context.Context, task *tasks.Task) error {
	hadReservation := s.gate.HasProtectedCacheReservation(task.ID)
	if err := s.gate.ReleaseProtectedCache(ctx, task.ID); err != nil {
		return err
	}
	task.ProtectedCacheBytes = 0
	if err := s.tasks.SetProtectedCacheBytes(ctx, task.ID, 0); err != nil {
		return err
	}
	if hadReservation {
		if s.service != nil {
			if err := s.service.wakeAvailableQuotaWaiter(ctx, "protected_cache_capacity", s.gate.State()); err != nil && s.service.logger != nil {
				s.service.logger.Warn("protected cache quota waiter wake failed", "error_class", "storage")
			}
		}
	}
	return nil
}

func (s *solidifier) clearTaskProtection(task *tasks.Task) {
	task.ScopeID = ""
	task.SessionIDs = nil
	task.RequestedBytes = 0
	task.ReservedBytes = 0
	task.GCProtected = false
	task.RecoveryReason = ""
	task.ProtectionSessionID = ""
	task.ProtectionOwnerID = ""
}
