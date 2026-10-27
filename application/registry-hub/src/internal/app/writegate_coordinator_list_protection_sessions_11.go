package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
)

func (g *writeGate) ListProtectionSessions(ctx context.Context, after string, limit int) ([]protectionSessionStatus, string, error) {
	if after != "" && !validGateID(after) {
		return nil, "", errors.New("protection session page token is invalid")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	items := make([]protectionSessionStatus, 0, limit)
	next := ""
	for _, id := range g.state.SessionIDs {
		if after != "" && id <= after {
			continue
		}
		if len(items) == limit {
			next = items[len(items)-1].ID
			break
		}
		status, err := g.protectionSessionStatusLocked(ctx, id)
		if err != nil {
			return nil, "", err
		}
		items = append(items, status)
	}
	return items, next, nil
}

func (g *writeGate) ProtectionSessionStatus(ctx context.Context, sessionID string) (protectionSessionStatus, error) {
	if !validGateID(sessionID) {
		return protectionSessionStatus{}, errors.New("protection session ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		session, err := g.readProtectionSession(ctx, sessionID)
		if err != nil {
			return protectionSessionStatus{}, err
		}
		if session.State != "released" {
			return protectionSessionStatus{}, storage.ErrNotFound
		}
		return g.protectionSessionStatusFromRecordLocked(ctx, session)
	}
	return g.protectionSessionStatusLocked(ctx, sessionID)
}

func (g *writeGate) protectionSessionStatusLocked(ctx context.Context, sessionID string) (protectionSessionStatus, error) {
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return protectionSessionStatus{}, err
	}
	if session.State == "released" {
		return protectionSessionStatus{}, errors.New("active protection session is marked released")
	}
	return g.protectionSessionStatusFromRecordLocked(ctx, session)
}

func (g *writeGate) protectionSessionStatusFromRecordLocked(ctx context.Context, session protectionSessionRecord) (protectionSessionStatus, error) {
	ownerCount, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
	if err != nil {
		return protectionSessionStatus{}, err
	}
	status := protectionSessionStatus{
		ID: session.ID, ScopeID: session.ScopeID, ReservationClass: session.ReservationClass, State: session.State, OwnerCount: ownerCount,
		ReservedBytes: session.ReservedBytes, PendingOperations: session.PendingOperations,
		UncertainRemoteResult: session.UncertainRemoteResult, MayContainUnanchored: session.MayContainUnanchored,
		CancelPhase: session.CancelPhase, RecoveryReason: session.RecoveryReason,
	}
	if session.RemoteWrite != nil {
		status.RemoteOperationKind = session.RemoteWrite.Kind
		status.RemoteDigest = session.RemoteWrite.Digest
		status.RemoteSubject = session.RemoteWrite.Subject
	}
	return status, nil
}

func (g *writeGate) ProtectionSessionOwners(ctx context.Context, sessionID string) ([]string, error) {
	if !validGateID(sessionID) {
		return nil, errors.New("protection session ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return nil, storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return g.listProtectionOwnersLocked(ctx, session.OwnerSetID)
}

func (g *writeGate) ListProtectionSessionOwners(ctx context.Context, sessionID, token string, limit int) ([]string, string, error) {
	if !validGateID(sessionID) || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, "", errors.New("protection owner page identity is invalid")
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return nil, "", storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	prefix := "system/solidification/owners/" + session.OwnerSetID + "/"
	entries, next, err := g.store.List(ctx, prefix, token, limit)
	if err != nil {
		return nil, "", err
	}
	owners := make([]string, 0, len(entries))
	for _, entry := range entries {
		ownerID := strings.TrimSuffix(strings.TrimPrefix(entry.Key, prefix), ".json")
		if !validGateID(ownerID) {
			return nil, "", errors.New("protection owner index contains an invalid demand ID")
		}
		if err := g.verifyProtectionOwner(ctx, entry.Key, session.OwnerSetID, ownerID); err != nil {
			return nil, "", err
		}
		owners = append(owners, ownerID)
	}
	return owners, next, nil
}

func protectionSessionKey(id string) string { return "system/solidification/sessions/" + id + ".json" }

func protectionOwnerKey(ownerSetID, demandID string) string {
	return "system/solidification/owners/" + ownerSetID + "/" + demandID + ".json"
}

func validGateID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, value := range id {
		if value < '0' || value > '9' {
			if value < 'a' || value > 'f' {
				return false
			}
		}
	}
	return true
}

func containsString(values []string, value string) bool { return stringIndex(values, value) >= 0 }

func stringIndex(values []string, value string) int {
	for index, item := range values {
		if item == value {
			return index
		}
	}
	return -1
}

func cloneGateRecord(source gateRecord) gateRecord {
	copy := source
	copy.SessionIDs = append([]string(nil), source.SessionIDs...)
	if source.ProtectedCacheReservations != nil {
		copy.ProtectedCacheReservations = make(map[string]int64, len(source.ProtectedCacheReservations))
		for taskID, reserved := range source.ProtectedCacheReservations {
			copy.ProtectedCacheReservations[taskID] = reserved
		}
	}
	return copy
}
