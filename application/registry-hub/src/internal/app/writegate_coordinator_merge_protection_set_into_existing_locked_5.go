package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (g *writeGate) mergeProtectionSetIntoExistingLocked(ctx context.Context, oldIDs []string, newSessionID, scopeID, demandID string, reservedBytes int64) error {
	current, err := g.readProtectionSession(ctx, newSessionID)
	if err != nil {
		return err
	}
	if current.ScopeID != scopeID || current.ReservedBytes != reservedBytes || current.ReservationClass != "new" || current.State != "protected" && current.State != "anchoring" {
		return errors.New("existing replacement protection conflicts with the requested scope")
	}
	oldSessions := make([]protectionSessionRecord, 0, len(oldIDs))
	oldOwners := make(map[string]struct{})
	var oldNewBytes, oldLegacyBytes int64
	oldNewCount := 0
	for _, id := range oldIDs {
		if !containsString(g.state.SessionIDs, id) {
			continue
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
	if len(oldSessions) == 0 {
		if demandID != "" {
			return g.linkProtectionOwnerLocked(ctx, current, demandID)
		}
		return nil
	}
	if oldNewCount > g.state.NewProtectionSessionCount {
		return errors.New("protection coordinator new-session count is invalid")
	}
	for owner := range oldOwners {
		if err := g.linkProtectionOwnerLocked(ctx, current, owner); err != nil {
			return err
		}
	}
	if demandID != "" {
		if err := g.linkProtectionOwnerLocked(ctx, current, demandID); err != nil {
			return err
		}
	}
	previous := cloneGateRecord(g.state)
	for _, session := range oldSessions {
		index := stringIndex(g.state.SessionIDs, session.ID)
		g.state.SessionIDs = append(g.state.SessionIDs[:index], g.state.SessionIDs[index+1:]...)
	}
	g.state.NewProtectionSessionCount -= oldNewCount
	if err := adjustReservationTotals(&g.state, "new", -oldNewBytes); err != nil {
		g.state = previous
		return err
	}
	if err := adjustReservationTotals(&g.state, "legacy", -oldLegacyBytes); err != nil {
		g.state = previous
		return err
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("protection_transfer_unknown")
			return errors.Join(err, reloadErr)
		}
		for _, session := range oldSessions {
			if containsString(g.state.SessionIDs, session.ID) {
				return err
			}
		}
	}
	for _, session := range oldSessions {
		session.State = "transferred"
		session.TerminalReason = "scope_merged"
		if err := g.saveProtectionSession(ctx, &session); err != nil {
			g.failClosedLocked("protection_transfer_unknown")
			return err
		}
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) listProtectionOwnersLocked(ctx context.Context, ownerSetID string) ([]string, error) {
	var owners []string
	token := ""
	prefix := "system/solidification/owners/" + ownerSetID + "/"
	for {
		entries, next, err := g.store.List(ctx, prefix, token, 1000)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			ownerID := strings.TrimSuffix(strings.TrimPrefix(entry.Key, prefix), ".json")
			if !validGateID(ownerID) {
				return nil, errors.New("protection owner index contains an invalid demand ID")
			}
			if err := g.verifyProtectionOwner(ctx, entry.Key, ownerSetID, ownerID); err != nil {
				return nil, err
			}
			owners = append(owners, ownerID)
		}
		if next == "" {
			return owners, nil
		}
		token = next
	}
}

func (g *writeGate) AddProtectionOwner(ctx context.Context, sessionID, demandID string) error {
	return g.addProtectionOwner(ctx, sessionID, demandID, false)
}

func (g *writeGate) AddProtectionOwnerDuringRecovery(ctx context.Context, sessionID, demandID string) error {
	return g.addProtectionOwner(ctx, sessionID, demandID, true)
}

func (g *writeGate) addProtectionOwner(ctx context.Context, sessionID, demandID string, recovery bool) error {
	if !validGateID(sessionID) || !validGateID(demandID) {
		return errors.New("protection session and demand IDs must be valid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.RecoveryBarrier != recovery {
		return ErrRecoveryBarrier
	}
	if !containsString(g.state.SessionIDs, sessionID) || g.state.State != "open" {
		return storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if session.State != "protected" && session.State != "anchoring" {
		return errors.New("protection session no longer accepts demand owners")
	}
	return g.linkProtectionOwnerLocked(ctx, session, demandID)
}
