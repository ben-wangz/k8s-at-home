package app

import (
	"context"
	"errors"
	"time"
)

func (g *writeGate) reopenProtectionSessionLocked(ctx context.Context, session *protectionSessionRecord, demandID string) error {
	if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil {
		return ErrProtectionBusy
	}
	if demandID != "" {
		if err := g.linkProtectionOwnerLocked(ctx, *session, demandID); err != nil {
			return err
		}
	}
	session.State = "protected"
	session.TerminalReason = ""
	session.ReservationID = ""
	session.LastProgressAt = time.Now().UTC()
	session.StopRequestedAt = time.Time{}
	session.PendingOperations = 0
	session.UncertainRemoteResult = false
	session.RemoteWrite = nil
	session.MayContainUnanchored = true
	session.CancelPhase = ""
	session.RecoveryStage = ""
	session.RecoveryReason = ""
	if err := g.saveProtectionSession(ctx, session); err != nil {
		g.failClosedLocked("protection_reopen_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) BeginStartupRecovery(ctx context.Context) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.RecoveryBarrier {
		return true, nil
	}
	g.state.RecoveryBarrier = true
	g.state.RecoveryReason = "startup_reconcile"
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("startup_recovery_unknown")
		return false, err
	}
	g.signalLocked()
	return true, nil
}

func (g *writeGate) ValidateProtectionState(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.validateProtectionStateLocked(ctx, false)
}

func (g *writeGate) CompleteStartupRecovery(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.state.RecoveryBarrier {
		return errors.New("recovery barrier is not active")
	}
	if g.active != 0 {
		return ErrProtectionBusy
	}
	if err := g.validateProtectionStateLocked(ctx, true); err != nil {
		return err
	}
	g.state.RecoveryBarrier = false
	g.state.RecoveryReason = ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("startup_recovery_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) validateProtectionStateLocked(ctx context.Context, requireQuiescent bool) error {
	if g.state.ReservedDistributionBytes < 0 || g.state.ProtectedCacheBytes < 0 || g.state.LegacyGrandfatheredBytes < 0 {
		return errors.New("protection reservation totals are negative")
	}
	seen := make(map[string]struct{}, len(g.state.SessionIDs))
	var newTotal, legacyTotal int64
	newSessionCount := 0
	for _, id := range g.state.SessionIDs {
		if !validGateID(id) {
			return errors.New("protection coordinator contains an invalid session ID")
		}
		if _, exists := seen[id]; exists {
			return errors.New("protection coordinator contains a duplicate session ID")
		}
		seen[id] = struct{}{}
		session, err := g.readProtectionSession(ctx, id)
		if err != nil {
			return err
		}
		currentTotal := newTotal
		if session.ReservationClass == "legacy" {
			currentTotal = legacyTotal
		}
		if session.State == "released" || session.ReservedBytes > int64(^uint64(0)>>1)-currentTotal {
			return errors.New("protection session state or reservation total is invalid")
		}
		if session.ReservationClass == "legacy" {
			legacyTotal += session.ReservedBytes
		} else {
			newTotal += session.ReservedBytes
			newSessionCount++
		}
		if requireQuiescent && (session.PendingOperations != 0 || session.UncertainRemoteResult || session.State != "protected" && session.State != "anchored") {
			return ErrRecoveryBarrier
		}
	}
	if newTotal != g.state.ReservedDistributionBytes || legacyTotal != g.state.LegacyGrandfatheredBytes || newSessionCount != g.state.NewProtectionSessionCount {
		return errors.New("protection coordinator reservations do not match active sessions")
	}
	var cacheTotal int64
	for taskID, reserved := range g.state.ProtectedCacheReservations {
		if !validGateID(taskID) || reserved < 0 || reserved > int64(^uint64(0)>>1)-cacheTotal {
			return errors.New("protected cache reservation is invalid")
		}
		cacheTotal += reserved
	}
	if len(g.state.ProtectedCacheReservations) > 10000 || g.state.ProtectedCacheReservationCount != len(g.state.ProtectedCacheReservations) || cacheTotal != g.state.ProtectedCacheBytes {
		return errors.New("protected cache reservation total does not match coordinator state")
	}
	return nil
}

func adjustReservationTotals(state *gateRecord, reservationClass string, delta int64) error {
	var total *int64
	switch reservationClass {
	case "new":
		total = &state.ReservedDistributionBytes
	case "legacy":
		total = &state.LegacyGrandfatheredBytes
	default:
		return errors.New("protection reservation class is invalid")
	}
	if delta < 0 {
		amount := uint64(-(delta + 1)) + 1
		if amount > uint64(*total) {
			return errors.New("protection reservation exceeds its coordinator total")
		}
		*total -= int64(amount)
		return nil
	}
	if delta > int64(^uint64(0)>>1)-*total {
		return errors.New("protection reservation total overflows")
	}
	*total += delta
	return nil
}

func (g *writeGate) newProtectionSessionCountLocked(ctx context.Context) (int, error) {
	return g.state.NewProtectionSessionCount, nil
}
