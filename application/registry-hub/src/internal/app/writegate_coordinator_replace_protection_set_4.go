package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

func (g *writeGate) ReplaceProtectionSet(ctx context.Context, oldSessionIDs []string, newSessionID, scopeID, demandID string, reservedBytes int64, maxSessions int, maxBytes int64) error {
	if len(oldSessionIDs) == 0 || !validGateID(newSessionID) || scopeID == "" || demandID != "" && !validGateID(demandID) || reservedBytes < 0 || maxSessions < 1 || maxSessions > 4096 || maxBytes < 1 {
		return errors.New("solidification protection set replacement is invalid")
	}
	oldIDs := append([]string(nil), oldSessionIDs...)
	sort.Strings(oldIDs)
	for index, id := range oldIDs {
		if !validGateID(id) || id == newSessionID || index > 0 && oldIDs[index-1] == id {
			return errors.New("solidification protection replacement contains an invalid session ID")
		}
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
		return g.mergeProtectionSetIntoExistingLocked(ctx, oldIDs, newSessionID, scopeID, demandID, reservedBytes)
	}
	oldSessions := make([]protectionSessionRecord, 0, len(oldIDs))
	oldOwners := make(map[string]struct{})
	var oldNewBytes, oldLegacyBytes int64
	oldNewCount := 0
	for _, id := range oldIDs {
		if !containsString(g.state.SessionIDs, id) {
			return storage.ErrNotFound
		}
		session, err := g.readProtectionSession(ctx, id)
		if err != nil {
			return err
		}
		if session.State != "protected" && session.State != "anchoring" || session.PendingOperations != 0 || session.UncertainRemoteResult {
			return ErrProtectionBusy
		}
		if session.ReservationClass == "legacy" {
			if session.ReservedBytes > g.state.LegacyGrandfatheredBytes-oldLegacyBytes {
				return errors.New("source legacy protection reservation exceeds coordinator total")
			}
			oldLegacyBytes += session.ReservedBytes
		} else {
			if session.ReservedBytes > g.state.ReservedDistributionBytes-oldNewBytes {
				return errors.New("source protection reservation exceeds coordinator total")
			}
			oldNewBytes += session.ReservedBytes
			oldNewCount++
		}
		oldSessions = append(oldSessions, session)
		owners, err := g.listProtectionOwnersLocked(ctx, session.OwnerSetID)
		if err != nil {
			return err
		}
		for _, owner := range owners {
			oldOwners[owner] = struct{}{}
		}
	}
	remainingBytes := g.state.ReservedDistributionBytes - oldNewBytes
	newCount, err := g.newProtectionSessionCountLocked(ctx)
	if err != nil {
		return err
	}
	if oldNewCount > newCount {
		return errors.New("protection coordinator new-session count is invalid")
	}
	if reservedBytes > maxBytes-remainingBytes || newCount-oldNewCount+1 > maxSessions {
		return ErrProtectionCapacity
	}
	newSession, exists, _, err := g.prepareProtectionSessionLocked(ctx, newSessionID, scopeID, reservedBytes, false)
	if err != nil {
		return err
	}
	if exists && newSession.ReservationClass != "new" {
		return errors.New("replacement protection session has an incompatible reservation class")
	}
	if !exists {
		newSession = protectionSessionRecord{
			SchemaVersion: 1, ID: newSessionID, ScopeID: scopeID, OwnerSetID: newSessionID, ReservationClass: "new",
			State: "protected", ReservedBytes: reservedBytes, CreatedAt: time.Now().UTC(), LastProgressAt: time.Now().UTC(),
		}
		if err := g.createProtectionSession(ctx, &newSession); err != nil {
			return err
		}
	}
	for owner := range oldOwners {
		if err := g.linkProtectionOwnerLocked(ctx, newSession, owner); err != nil {
			g.discardProtectionSession(newSession)
			return err
		}
	}
	if demandID != "" {
		if err := g.linkProtectionOwnerLocked(ctx, newSession, demandID); err != nil {
			g.discardProtectionSession(newSession)
			return err
		}
	}
	previous := cloneGateRecord(g.state)
	for _, id := range oldIDs {
		index := stringIndex(g.state.SessionIDs, id)
		g.state.SessionIDs = append(g.state.SessionIDs[:index], g.state.SessionIDs[index+1:]...)
	}
	g.state.SessionIDs = append(g.state.SessionIDs, newSessionID)
	sort.Strings(g.state.SessionIDs)
	g.state.NewProtectionSessionCount = newCount - oldNewCount + 1
	if err := adjustReservationTotals(&g.state, "new", -oldNewBytes); err != nil {
		g.state = previous
		return err
	}
	if err := adjustReservationTotals(&g.state, "legacy", -oldLegacyBytes); err != nil {
		g.state = previous
		return err
	}
	if err := adjustReservationTotals(&g.state, "new", reservedBytes); err != nil {
		g.state = previous
		return err
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reconcileErr := g.reloadLocked(ctx); reconcileErr != nil {
			g.state = previous
			g.failClosedLocked("protection_transfer_unknown")
			return errors.Join(err, reconcileErr)
		}
		if containsString(g.state.SessionIDs, newSessionID) {
			for _, id := range oldIDs {
				if containsString(g.state.SessionIDs, id) {
					g.failClosedLocked("protection_transfer_unknown")
					return errors.Join(err, errors.New("replacement result is inconsistent"))
				}
			}
		} else {
			for _, id := range oldIDs {
				if containsString(g.state.SessionIDs, id) {
					g.discardProtectionSession(newSession)
					g.signalLocked()
					return err
				}
			}
			g.failClosedLocked("protection_transfer_unknown")
			return err
		}
	}
	var terminalErr error
	for _, old := range oldSessions {
		old.State = "transferred"
		old.TerminalReason = "scope_merged"
		if err := g.saveProtectionSession(ctx, &old); err != nil {
			terminalErr = errors.Join(terminalErr, err)
		}
	}
	g.signalLocked()
	return terminalErr
}
