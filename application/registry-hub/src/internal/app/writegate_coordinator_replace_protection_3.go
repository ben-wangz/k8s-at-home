package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

func (g *writeGate) ReplaceProtection(ctx context.Context, oldSessionID, newSessionID, scopeID string, reservedBytes int64, maxSessions int, maxBytes int64) error {
	if !validGateID(oldSessionID) || !validGateID(newSessionID) || oldSessionID == newSessionID || scopeID == "" || len(scopeID) > 1024 || strings.ContainsAny(scopeID, "\r\n\x00") || reservedBytes < 0 || maxSessions < 1 || maxSessions > 4096 || maxBytes < 1 {
		return errors.New("solidification protection replacement is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if containsString(g.state.SessionIDs, newSessionID) {
		current, err := g.readProtectionSession(ctx, newSessionID)
		if err != nil {
			return err
		}
		if current.ScopeID != scopeID || current.ReservedBytes != reservedBytes || current.ReservationClass != "new" || containsString(g.state.SessionIDs, oldSessionID) {
			return errors.New("existing replacement protection conflicts with the requested scope")
		}
		return nil
	}
	oldIndex := stringIndex(g.state.SessionIDs, oldSessionID)
	if oldIndex < 0 {
		return storage.ErrNotFound
	}
	oldSession, err := g.readProtectionSession(ctx, oldSessionID)
	if err != nil {
		return err
	}
	oldTotal := g.state.ReservedDistributionBytes
	if oldSession.ReservationClass == "legacy" {
		oldTotal = g.state.LegacyGrandfatheredBytes
	}
	if oldSession.State == "released" || oldSession.PendingOperations != 0 || oldSession.ReservedBytes > oldTotal {
		return errors.New("old protection session cannot be transferred safely")
	}
	remainingNew := g.state.ReservedDistributionBytes
	if oldSession.ReservationClass == "new" {
		remainingNew -= oldSession.ReservedBytes
	}
	newCount, err := g.newProtectionSessionCountLocked(ctx)
	if err != nil {
		return err
	}
	if oldSession.ReservationClass == "new" {
		if newCount < 1 {
			return errors.New("protection coordinator new-session count is invalid")
		}
		newCount--
	}
	if newCount+1 > maxSessions || reservedBytes > maxBytes-remainingNew {
		return ErrProtectionCapacity
	}
	newSession := protectionSessionRecord{
		SchemaVersion: 1, ID: newSessionID, ScopeID: scopeID, OwnerSetID: oldSession.OwnerSetID,
		ReservationClass: "new", State: "protected", ReservedBytes: reservedBytes, CreatedAt: time.Now().UTC(), LastProgressAt: time.Now().UTC(),
	}
	if err := g.createProtectionSession(ctx, &newSession); err != nil {
		return err
	}
	previous := cloneGateRecord(g.state)
	g.state.SessionIDs[oldIndex] = newSessionID
	sort.Strings(g.state.SessionIDs)
	g.state.NewProtectionSessionCount = newCount + 1
	if err := adjustReservationTotals(&g.state, oldSession.ReservationClass, -oldSession.ReservedBytes); err != nil {
		g.state = previous
		g.discardProtectionSession(newSession)
		return err
	}
	if err := adjustReservationTotals(&g.state, "new", reservedBytes); err != nil {
		g.state = previous
		g.discardProtectionSession(newSession)
		return err
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		return g.reconcileReplacementFailure(ctx, previous, oldSession, newSession, err)
	}
	oldSession.State = "transferred"
	oldSession.TerminalReason = "scope_merged"
	_ = g.saveProtectionSessionBounded(oldSession)
	g.signalLocked()
	return nil
}
