package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (g *writeGate) RemoveProtectionOwner(ctx context.Context, sessionID, demandID string) (int, error) {
	if !validGateID(sessionID) || !validGateID(demandID) {
		return 0, errors.New("protection session and demand IDs must be valid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return 0, storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	ownerKey := protectionOwnerKey(session.OwnerSetID, demandID)
	if entry, err := g.store.Head(ctx, ownerKey); err == nil {
		if err := g.store.DeleteIfMatch(ctx, ownerKey, entry.Version); err != nil && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrNotFound) {
			return 0, err
		} else if err == nil {
			g.signalLocked()
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return 0, err
	}
	return g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
}

func (g *writeGate) RequestProtectionStop(ctx context.Context, sessionID, reason string) (bool, error) {
	if !validGateID(sessionID) || reason != "cancelled" && reason != "expired" && reason != "failed" {
		return false, errors.New("protection stop request is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return false, storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return false, err
	}
	owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
	if err != nil {
		return false, err
	}
	if owners > 0 {
		return false, nil
	}
	if session.State == "anchored" || session.State == "quiesced" || session.State == "released" {
		return true, nil
	}
	if session.State != "stop_requested" {
		session.State = "stop_requested"
		session.TerminalReason = reason
		session.CancelPhase = "stop_requested"
		session.StopRequestedAt = time.Now().UTC()
		if err := g.saveProtectionSession(ctx, &session); err != nil {
			return false, err
		}
		g.signalLocked()
	}
	return true, nil
}

func (g *writeGate) MarkProtectionAnchored(ctx context.Context, sessionID string) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return errors.New("protection session cannot be anchored while stopping or while operations remain")
		}
		if session.State == "anchored" {
			return nil
		}
		if session.State != "protected" && session.State != "anchoring" {
			return errors.New("protection session cannot be anchored while stopping or while operations remain")
		}
		session.State = "anchored"
		session.CancelPhase = ""
		session.TerminalReason = "anchored"
		session.RecoveryStage = ""
		session.RecoveryReason = ""
		return nil
	})
}

func (g *writeGate) MarkProtectionWriteStarted(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	if !validProtectionRemoteWrite(remoteWrite) {
		return errors.New("protection remote write identity is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "protected" && session.State != "anchoring" {
			return errors.New("protection session no longer admits remote writes")
		}
		if session.UncertainRemoteResult || session.RemoteWrite != nil {
			return ErrProtectionBusy
		}
		session.UncertainRemoteResult = true
		writeCopy := remoteWrite
		session.RemoteWrite = &writeCopy
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) MarkProtectionWriteResolved(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if !session.UncertainRemoteResult {
			return nil
		}
		if session.RemoteWrite == nil || *session.RemoteWrite != remoteWrite {
			return errors.New("protection remote write identity does not match the unresolved operation")
		}
		session.UncertainRemoteResult = false
		session.RemoteWrite = nil
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) CompleteProtectionStartupRecovery(ctx context.Context, sessionID string, verifiedWrite *protectionRemoteWrite) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if verifiedWrite != nil {
			if !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != *verifiedWrite {
				return errors.New("protection remote write changed during startup recovery")
			}
			session.UncertainRemoteResult = false
			session.RemoteWrite = nil
		} else if session.UncertainRemoteResult || session.RemoteWrite != nil {
			return errors.New("protection session still has an unresolved remote write")
		}
		session.PendingOperations = 0
		session.RecoveryStage = "remote_operations_reconciled"
		session.RecoveryReason = ""
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) SetProtectionRecoveryRequired(ctx context.Context, sessionID, reason string) error {
	if reason == "" || len(reason) > 64 || strings.ContainsAny(reason, "\r\n\x00") {
		return errors.New("protection recovery reason is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		session.RecoveryStage = "remote_result_unknown"
		session.RecoveryReason = reason
		return nil
	})
}

func (g *writeGate) MarkProtectionContentObserved(ctx context.Context, sessionID string) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "protected" && session.State != "anchoring" {
			return errors.New("protection session no longer records content observations")
		}
		session.MayContainUnanchored = true
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) MarkRecoveredProtectionContentObserved(ctx context.Context, sessionID string) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "protected" && session.State != "anchoring" && session.State != "stop_requested" && session.State != "quiescing" && session.State != "recovery_required" {
			return errors.New("recovered protection session cannot record content observations")
		}
		session.MayContainUnanchored = true
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) protectionSession(ctx context.Context, sessionID string) (protectionSessionRecord, bool, error) {
	if !validGateID(sessionID) {
		return protectionSessionRecord{}, false, errors.New("protection session ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return protectionSessionRecord{}, false, nil
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	return session, err == nil, err
}

func (g *writeGate) confirmInactiveProtectionSessionReleased(ctx context.Context, sessionID, expectedScopeID, expectedReservationClass string) error {
	if !validGateID(sessionID) {
		return errors.New("protection session ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.reloadLocked(ctx); err != nil {
		g.failClosedLocked("protection_release_reconcile_unknown")
		return err
	}
	if containsString(g.state.SessionIDs, sessionID) {
		return ErrRecoveryBarrier
	}
	if err := g.validateProtectionStateLocked(ctx, false); err != nil {
		return err
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if (expectedScopeID != "" && session.ScopeID != expectedScopeID) || (expectedReservationClass != "" && session.ReservationClass != expectedReservationClass) {
		return errors.New("inactive protection session identity does not match the task reference")
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil {
		return ErrProtectionBusy
	}
	if session.RecoveryStage != "" || session.RecoveryReason != "" {
		return ErrRecoveryBarrier
	}
	if session.State == "released" {
		if !validReleasedProtectionReason(session.TerminalReason) {
			return errors.New("released protection session has an invalid terminal reason")
		}
		if session.TerminalReason != "anchored" {
			owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
			if err != nil {
				return err
			}
			if owners != 0 {
				return errors.New("released protection session still has demand owners")
			}
		}
		return nil
	}
	switch {
	case session.State == "anchored" && session.TerminalReason == "anchored":
	case session.State == "quiesced" && validStopProtectionReason(session.TerminalReason):
		owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
		if err != nil {
			return err
		}
		if owners != 0 {
			return errors.New("quiesced protection session still has demand owners")
		}
	default:
		return errors.New("inactive protection session is not safely terminal")
	}
	session.State = "released"
	session.CancelPhase = "released"
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.failClosedLocked("protection_release_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func validReleasedProtectionReason(reason string) bool {
	return reason == "anchored" || reason == "cancelled" || reason == "expired" || reason == "failed" || reason == "empty"
}

func validStopProtectionReason(reason string) bool {
	return reason == "cancelled" || reason == "expired" || reason == "failed"
}
