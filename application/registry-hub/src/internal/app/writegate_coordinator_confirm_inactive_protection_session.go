package app

import (
	"context"
	"errors"
)

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
	if session.State == "transferred" {
		if session.TerminalReason != "scope_merged" {
			return errors.New("transferred protection session has an invalid terminal reason")
		}
		return nil
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
