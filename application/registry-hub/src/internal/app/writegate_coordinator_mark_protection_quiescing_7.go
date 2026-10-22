package app

import (
	"context"
	"errors"
	"strings"
	"time"
)

func (g *writeGate) MarkProtectionQuiescing(ctx context.Context, sessionID string) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "stop_requested" && session.State != "recovery_required" {
			return errors.New("protection session has no stop request")
		}
		session.State = "quiescing"
		session.CancelPhase = "quiescing"
		return nil
	})
}

func (g *writeGate) MarkProtectionQuiesced(ctx context.Context, sessionID string) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "stop_requested" && session.State != "quiescing" && session.State != "recovery_required" {
			return errors.New("protection session is not stopping")
		}
		if session.PendingOperations != 0 {
			return errors.New("protection session still has in-flight operations")
		}
		session.State = "quiesced"
		session.CancelPhase = "quiesced"
		return nil
	})
}

func (g *writeGate) MarkProtectionRecoveryRequired(ctx context.Context, sessionID, reason string) error {
	if reason == "" || len(reason) > 64 || strings.ContainsAny(reason, "\r\n\x00") {
		return errors.New("protection recovery reason is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		session.State = "recovery_required"
		session.CancelPhase = "recovery_required"
		session.RecoveryStage = "remote_result"
		session.RecoveryReason = reason
		return nil
	})
}

func (g *writeGate) ReleaseProtection(ctx context.Context, sessionID, reason string) error {
	if !validGateID(sessionID) || reason != "anchored" && reason != "cancelled" && reason != "expired" && reason != "failed" {
		return errors.New("protection release is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	index := stringIndex(g.state.SessionIDs, sessionID)
	if index < 0 {
		return nil
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return errors.New("protection session cannot be released before quiescence")
	}
	currentTotal := g.state.ReservedDistributionBytes
	if session.ReservationClass == "legacy" {
		currentTotal = g.state.LegacyGrandfatheredBytes
	}
	if session.ReservedBytes > currentTotal {
		return errors.New("protection session reservation exceeds coordinator total")
	}
	if reason == "anchored" && session.State != "anchored" || reason != "anchored" && session.State != "quiesced" {
		return errors.New("protection session has not reached a safe terminal state")
	}
	if reason != "anchored" {
		owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
		if err != nil {
			return err
		}
		if owners != 0 {
			return errors.New("protection session still has valid demand owners")
		}
	}
	previous := cloneGateRecord(g.state)
	g.state.SessionIDs = append(g.state.SessionIDs[:index], g.state.SessionIDs[index+1:]...)
	if session.ReservationClass == "new" {
		if g.state.NewProtectionSessionCount < 1 {
			g.state = previous
			return errors.New("protection coordinator new-session count is invalid")
		}
		g.state.NewProtectionSessionCount--
	}
	if err := adjustReservationTotals(&g.state, session.ReservationClass, -session.ReservedBytes); err != nil {
		g.state = previous
		return err
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reconcileErr := g.reloadLocked(ctx); reconcileErr != nil {
			g.failClosedLocked("protection_release_unknown")
			return errors.Join(err, reconcileErr)
		}
		if containsString(g.state.SessionIDs, sessionID) {
			g.state = previous
			return err
		}
	}
	session.State = "released"
	session.CancelPhase = "released"
	session.TerminalReason = reason
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}
