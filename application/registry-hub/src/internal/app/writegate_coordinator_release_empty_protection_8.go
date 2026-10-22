package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"time"
)

func (g *writeGate) ReleaseEmptyProtection(ctx context.Context, sessionID, demandID string) error {
	if !validGateID(sessionID) || !validGateID(demandID) {
		return errors.New("empty protection release identity is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return nil
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.MayContainUnanchored {
		return ErrProtectionHasContent
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	if session.State != "protected" && session.State != "anchoring" {
		return errors.New("empty protection session is not releasable")
	}
	ownerKey := protectionOwnerKey(session.OwnerSetID, demandID)
	if entry, err := g.store.Head(ctx, ownerKey); err == nil {
		if err := g.store.DeleteIfMatch(ctx, ownerKey, entry.Version); err != nil && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
	if err != nil || owners > 0 {
		return err
	}
	currentTotal := g.state.ReservedDistributionBytes
	if session.ReservationClass == "legacy" {
		currentTotal = g.state.LegacyGrandfatheredBytes
	}
	if session.ReservedBytes > currentTotal {
		return errors.New("empty protection reservation exceeds coordinator total")
	}
	index := stringIndex(g.state.SessionIDs, sessionID)
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
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("empty_protection_release_unknown")
			return errors.Join(err, reloadErr)
		}
		if containsString(g.state.SessionIDs, sessionID) {
			return err
		}
	}
	session.State = "released"
	session.TerminalReason = "empty"
	session.CancelPhase = "released"
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.failClosedLocked("empty_protection_state_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) BeginProtectionOperation(ctx context.Context, sessionID string) (func(context.Context) error, error) {
	releaseWrite, err := g.Enter(ctx)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	if !containsString(g.state.SessionIDs, sessionID) || g.state.State != "open" {
		g.mu.Unlock()
		releaseWrite()
		return nil, storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		g.mu.Unlock()
		releaseWrite()
		return nil, err
	}
	if session.State != "protected" && session.State != "anchoring" {
		g.mu.Unlock()
		releaseWrite()
		return nil, errors.New("protection session does not admit new operations")
	}
	session.PendingOperations++
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.mu.Unlock()
		releaseWrite()
		return nil, err
	}
	g.signalLocked()
	g.mu.Unlock()
	var once bool
	var onceMu sync.Mutex
	return func(endCtx context.Context) error {
		onceMu.Lock()
		if once {
			onceMu.Unlock()
			return nil
		}
		once = true
		onceMu.Unlock()
		defer releaseWrite()
		g.mu.Lock()
		defer g.mu.Unlock()
		current, err := g.readProtectionSession(endCtx, sessionID)
		if err != nil {
			g.failClosedLocked("protection_operation_unknown")
			return err
		}
		if current.PendingOperations < 1 {
			g.failClosedLocked("protection_operation_count")
			return errors.New("protection operation counter underflow")
		}
		current.PendingOperations--
		if err := g.saveProtectionSession(endCtx, &current); err != nil {
			g.failClosedLocked("protection_operation_unknown")
			return err
		}
		g.signalLocked()
		return nil
	}, nil
}

func (g *writeGate) SetRecoveryBarrier(ctx context.Context, reason string) error {
	if reason == "" || len(reason) > 64 || strings.ContainsAny(reason, "\r\n\x00") {
		return errors.New("recovery barrier reason is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.RecoveryBarrier = true
	g.state.RecoveryReason = reason
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("recovery_barrier_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) ClearRecoveryBarrier(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.state.SessionIDs) > 0 {
		return ErrGCProtected
	}
	g.state.RecoveryBarrier = false
	g.state.RecoveryReason = ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("recovery_barrier_unknown")
		return err
	}
	g.signalLocked()
	return nil
}
