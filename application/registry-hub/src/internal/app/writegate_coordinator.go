package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"sync"
	"time"
)

type protectionSessionRecord struct {
	SchemaVersion         int                    `json:"schemaVersion"`
	ID                    string                 `json:"id"`
	ScopeID               string                 `json:"scopeID"`
	OwnerSetID            string                 `json:"ownerSetID"`
	ReservationClass      string                 `json:"reservationClass,omitempty"`
	State                 string                 `json:"state"`
	TerminalReason        string                 `json:"terminalReason,omitempty"`
	ReservationID         string                 `json:"reservationID,omitempty"`
	ReservedBytes         int64                  `json:"reservedBytes"`
	CreatedAt             time.Time              `json:"createdAt"`
	LastProgressAt        time.Time              `json:"lastProgressAt"`
	StopRequestedAt       time.Time              `json:"stopRequestedAt,omitempty"`
	PendingOperations     int                    `json:"pendingOperations"`
	UncertainRemoteResult bool                   `json:"uncertainRemoteResult,omitempty"`
	RemoteWrite           *protectionRemoteWrite `json:"remoteWrite,omitempty"`
	MayContainUnanchored  bool                   `json:"mayContainUnanchored,omitempty"`
	CancelPhase           string                 `json:"cancelPhase,omitempty"`
	RecoveryStage         string                 `json:"recoveryStage,omitempty"`
	RecoveryReason        string                 `json:"recoveryReason,omitempty"`
	version               string
}

type protectionRemoteWrite struct {
	Kind         string `json:"kind"`
	Path         string `json:"path"`
	FallbackPath string `json:"fallbackPath,omitempty"`
	Digest       string `json:"digest"`
	Subject      string `json:"subject,omitempty"`
	Size         int64  `json:"size"`
	MediaType    string `json:"mediaType,omitempty"`
}

type protectionSessionStatus struct {
	ID                    string `json:"id"`
	ScopeID               string `json:"scopeID"`
	ReservationClass      string `json:"reservationClass"`
	State                 string `json:"state"`
	OwnerCount            int    `json:"ownerCount"`
	ReservedBytes         int64  `json:"reservedBytes"`
	PendingOperations     int    `json:"pendingOperations"`
	UncertainRemoteResult bool   `json:"uncertainRemoteResult,omitempty"`
	MayContainUnanchored  bool   `json:"mayContainUnanchored,omitempty"`
	RemoteOperationKind   string `json:"remoteOperationKind,omitempty"`
	RemoteDigest          string `json:"remoteDigest,omitempty"`
	RemoteSubject         string `json:"remoteSubject,omitempty"`
	CancelPhase           string `json:"cancelPhase,omitempty"`
	RecoveryReason        string `json:"recoveryReason,omitempty"`
}

func (g *writeGate) AdmitProtection(ctx context.Context, sessionID, scopeID, demandID string, reservedBytes int64, maxSessions int, maxBytes int64) error {
	return g.admitProtection(ctx, sessionID, scopeID, demandID, reservedBytes, maxSessions, maxBytes, false, false)
}

func (g *writeGate) AdmitProtectionDuringRecovery(ctx context.Context, sessionID, scopeID, demandID string, reservedBytes int64, maxSessions int, maxBytes int64) error {
	return g.admitProtection(ctx, sessionID, scopeID, demandID, reservedBytes, maxSessions, maxBytes, true, false)
}

func (g *writeGate) ReopenAnchoredProtection(ctx context.Context, sessionID, scopeID, demandID string, reservedBytes int64, maxSessions int, maxBytes int64) error {
	return g.admitProtection(ctx, sessionID, scopeID, demandID, reservedBytes, maxSessions, maxBytes, false, true)
}

func (g *writeGate) admitProtection(ctx context.Context, sessionID, scopeID, demandID string, reservedBytes int64, maxSessions int, maxBytes int64, startupRecovery, reopenAnchored bool) error {
	if !validGateID(sessionID) || scopeID == "" || len(scopeID) > 1024 || strings.ContainsAny(scopeID, "\r\n\x00") || demandID != "" && !validGateID(demandID) || reservedBytes < 0 || maxSessions < 1 || maxSessions > 4096 || maxBytes < 1 {
		return errors.New("solidification protection admission is invalid")
	}
	reservationClass := "new"
	if startupRecovery {
		reservationClass = "legacy"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
	if g.state.RecoveryBarrier && !(startupRecovery && g.state.RecoveryReason == "startup_reconcile") {
		return ErrRecoveryBarrier
	}
	if containsString(g.state.SessionIDs, sessionID) {
		session, err := g.readProtectionSession(ctx, sessionID)
		if err != nil {
			return err
		}
		if session.ScopeID != scopeID || session.ReservedBytes != reservedBytes || session.ReservationClass != reservationClass || session.State == "released" {
			return errors.New("existing protection session conflicts with the requested admission")
		}
		if session.State == "anchored" {
			if reopenAnchored {
				return g.reopenProtectionSessionLocked(ctx, &session, demandID)
			}
			return ErrProtectionAnchored
		}
		if demandID != "" {
			return g.linkProtectionOwnerLocked(ctx, session, demandID)
		}
		return nil
	}
	if existing, err := g.readProtectionSession(ctx, sessionID); err == nil {
		if existing.ScopeID != scopeID || existing.ReservedBytes != reservedBytes || existing.ReservationClass != reservationClass {
			return errors.New("existing protection session conflicts with the requested admission")
		}
		if existing.State == "anchored" || existing.State == "released" && existing.TerminalReason == "anchored" {
			if !reopenAnchored {
				return ErrProtectionAnchored
			}
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if len(g.state.SessionIDs) >= 4096 || reservationClass == "new" && (g.state.NewProtectionSessionCount >= maxSessions || reservedBytes > maxBytes-g.state.ReservedDistributionBytes) || reservationClass == "legacy" && reservedBytes > int64(^uint64(0)>>1)-g.state.LegacyGrandfatheredBytes {
		return ErrProtectionCapacity
	}
	session, exists, needsReopen, err := g.prepareProtectionSessionLocked(ctx, sessionID, scopeID, reservedBytes, reopenAnchored)
	if err != nil {
		return err
	}
	if !exists {
		session = protectionSessionRecord{
			SchemaVersion: 1, ID: sessionID, ScopeID: scopeID, OwnerSetID: sessionID, ReservationClass: reservationClass, State: "protected",
			ReservedBytes: reservedBytes, CreatedAt: time.Now().UTC(), LastProgressAt: time.Now().UTC(),
		}
		if err := g.createProtectionSession(ctx, &session); err != nil {
			return err
		}
	}
	if demandID != "" {
		if err := g.linkProtectionOwnerLocked(ctx, session, demandID); err != nil {
			g.discardProtectionSession(session)
			return err
		}
	}
	previous := cloneGateRecord(g.state)
	g.state.SessionIDs = append(g.state.SessionIDs, sessionID)
	sort.Strings(g.state.SessionIDs)
	if err := adjustReservationTotals(&g.state, reservationClass, reservedBytes); err != nil {
		g.state = previous
		return err
	}
	if reservationClass == "new" {
		g.state.NewProtectionSessionCount++
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if !needsReopen {
			return g.reconcileAdmissionFailure(ctx, previous, session, err)
		}
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("protection_admission_unknown")
			return errors.Join(err, reloadErr)
		}
		if !containsString(g.state.SessionIDs, sessionID) {
			return err
		}
	}
	if needsReopen {
		current, readErr := g.readProtectionSession(ctx, sessionID)
		if readErr != nil {
			g.failClosedLocked("protection_reopen_unknown")
			return readErr
		}
		if current.State == "anchored" || current.State == "released" && current.TerminalReason == "anchored" {
			if err := g.reopenProtectionSessionLocked(ctx, &current, ""); err != nil {
				return err
			}
		} else if current.State != "protected" {
			g.failClosedLocked("protection_reopen_unknown")
			return errors.New("anchored protection session changed during re-admission")
		}
	}
	g.signalLocked()
	return nil
}

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

func (g *writeGate) reconcileAdmissionFailure(ctx context.Context, previous gateRecord, session protectionSessionRecord, cause error) error {
	if err := g.reloadLocked(ctx); err != nil {
		g.state = previous
		g.failClosedLocked("protection_admission_unknown")
		return errors.Join(cause, err)
	}
	if containsString(g.state.SessionIDs, session.ID) {
		current, err := g.readProtectionSession(ctx, session.ID)
		if err == nil && current.ScopeID == session.ScopeID && current.ReservedBytes == session.ReservedBytes {
			g.signalLocked()
			return nil
		}
		g.failClosedLocked("protection_admission_unknown")
		return errors.Join(cause, err)
	}
	g.discardProtectionSession(session)
	g.signalLocked()
	return cause
}

func (g *writeGate) reconcileReplacementFailure(ctx context.Context, previous gateRecord, oldSession, newSession protectionSessionRecord, cause error) error {
	if err := g.reloadLocked(ctx); err != nil {
		g.state = previous
		g.failClosedLocked("protection_transfer_unknown")
		return errors.Join(cause, err)
	}
	if containsString(g.state.SessionIDs, newSession.ID) && !containsString(g.state.SessionIDs, oldSession.ID) {
		g.signalLocked()
		oldSession.State = "transferred"
		oldSession.TerminalReason = "scope_merged"
		_ = g.saveProtectionSessionBounded(oldSession)
		return nil
	}
	if containsString(g.state.SessionIDs, oldSession.ID) {
		g.discardProtectionSession(newSession)
		g.signalLocked()
		return cause
	}
	g.failClosedLocked("protection_transfer_unknown")
	return cause
}

func (g *writeGate) reloadLocked(ctx context.Context) error {
	object, err := g.store.Get(ctx, "system/gc/state.json")
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || len(data) > 1<<20 {
		return errors.Join(errors.New("GC coordinator state could not be reloaded safely"), readErr, closeErr)
	}
	var latest gateRecord
	if err := json.Unmarshal(data, &latest); err != nil || latest.SchemaVersion != gateSchemaCurrent || latest.State == "" || latest.Epoch == 0 || latest.ReservedDistributionBytes < 0 || latest.ProtectedCacheBytes < 0 || latest.LegacyGrandfatheredBytes < 0 || latest.NewProtectionSessionCount < 0 || len(latest.SessionIDs) > 4096 || len(latest.ProtectedCacheReservations) > 10000 {
		return errors.New("GC coordinator state could not be validated after a conditional write")
	}
	if latest.ProtectedCacheReservations == nil {
		latest.ProtectedCacheReservations = make(map[string]int64)
	}
	if latest.ProtectedCacheReservationCount != len(latest.ProtectedCacheReservations) {
		return errors.New("GC coordinator protected cache reservation count is invalid")
	}
	latest.ProtectedCacheReservationCount = len(latest.ProtectedCacheReservations)
	g.state = latest
	g.version = object.Version
	return nil
}

func (g *writeGate) failClosedLocked(reason string) {
	g.state.State = "failed_unknown"
	g.state.ErrorClass = "storage"
	g.state.RecoveryBarrier = true
	g.state.RecoveryReason = reason
	g.state.UpdatedAt = time.Now().UTC()
	g.signalLocked()
}

func (g *writeGate) updateProtectionSession(ctx context.Context, sessionID string, update func(*protectionSessionRecord) error) error {
	if !validGateID(sessionID) {
		return errors.New("protection session ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !containsString(g.state.SessionIDs, sessionID) {
		return storage.ErrNotFound
	}
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := update(&session); err != nil {
		return err
	}
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.failClosedLocked("protection_state_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) createProtectionSession(ctx context.Context, session *protectionSessionRecord) error {
	if session == nil || session.ID == "" || session.OwnerSetID == "" {
		return errors.New("protection session is incomplete")
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	entry, err := g.store.Put(ctx, protectionSessionKey(session.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			existing, readErr := g.readProtectionSession(ctx, session.ID)
			if readErr == nil && existing.ScopeID == session.ScopeID && existing.OwnerSetID == session.OwnerSetID && existing.ReservationClass == session.ReservationClass && existing.ReservedBytes == session.ReservedBytes && existing.State != "released" {
				session.version = existing.version
				return nil
			}
			return errors.Join(err, readErr)
		}
		return err
	}
	session.version = entry.Version
	return nil
}

func (g *writeGate) readProtectionSession(ctx context.Context, id string) (protectionSessionRecord, error) {
	object, err := g.store.Get(ctx, protectionSessionKey(id))
	if err != nil {
		return protectionSessionRecord{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || len(data) > 1<<20 {
		return protectionSessionRecord{}, errors.Join(errors.New("protection session could not be read safely"), readErr, closeErr)
	}
	var session protectionSessionRecord
	if err := json.Unmarshal(data, &session); err != nil || session.SchemaVersion != 1 || session.ID != id || !validGateID(session.OwnerSetID) || session.ScopeID == "" || session.ReservedBytes < 0 || session.PendingOperations < 0 {
		return protectionSessionRecord{}, errors.New("protection session is invalid")
	}
	if session.RemoteWrite != nil && (!session.UncertainRemoteResult || !validProtectionRemoteWrite(*session.RemoteWrite)) {
		return protectionSessionRecord{}, errors.New("protection remote write state is invalid")
	}
	if session.ReservationClass == "" {
		session.ReservationClass = "new"
	}
	if session.ReservationClass != "new" && session.ReservationClass != "legacy" {
		return protectionSessionRecord{}, errors.New("protection session reservation class is invalid")
	}
	session.version = object.Version
	return session, nil
}

func validProtectionRemoteWrite(write protectionRemoteWrite) bool {
	if len(write.Path) < 5 || len(write.Path) > 8192 || !strings.HasPrefix(write.Path, "/v2/") || strings.ContainsAny(write.Path, "\r\n\x00") || !route.ValidDigest(write.Digest) || write.Size < 0 || len(write.MediaType) > 255 {
		return false
	}
	switch write.Kind {
	case "blob":
		return strings.Contains(write.Path, "/blobs/") && write.Subject == "" && write.FallbackPath == ""
	case "manifest", "root_tag":
		return strings.Contains(write.Path, "/manifests/") && write.Subject == "" && write.FallbackPath == ""
	case "referrer":
		return route.ValidDigest(write.Subject) && len(write.FallbackPath) <= 8192 && strings.HasPrefix(write.Path, "/v2/") && strings.Contains(write.Path, "/referrers/") && strings.HasPrefix(write.FallbackPath, "/v2/") && strings.Contains(write.FallbackPath, "/manifests/") && !strings.ContainsAny(write.FallbackPath, "\r\n\x00")
	default:
		return false
	}
}

func (g *writeGate) saveProtectionSession(ctx context.Context, session *protectionSessionRecord) error {
	if session == nil || session.version == "" {
		return errors.New("protection session has no storage version")
	}
	data, err := json.Marshal(session)
	if err != nil {
		return err
	}
	entry, err := g.store.Put(ctx, protectionSessionKey(session.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: session.version})
	if err != nil {
		return err
	}
	session.version = entry.Version
	return nil
}

func (g *writeGate) prepareProtectionSessionLocked(ctx context.Context, sessionID, scopeID string, reservedBytes int64, reopenAnchored bool) (protectionSessionRecord, bool, bool, error) {
	session, err := g.readProtectionSession(ctx, sessionID)
	if errors.Is(err, storage.ErrNotFound) {
		return protectionSessionRecord{}, false, false, nil
	}
	if err != nil {
		return protectionSessionRecord{}, false, false, err
	}
	if session.ScopeID != scopeID || session.ReservedBytes != reservedBytes {
		return protectionSessionRecord{}, false, false, errors.New("existing protection session conflicts with the requested scope")
	}
	if session.State == "anchored" || session.State == "released" && session.TerminalReason == "anchored" {
		if !reopenAnchored {
			return protectionSessionRecord{}, false, false, ErrProtectionAnchored
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil {
			return protectionSessionRecord{}, false, false, ErrProtectionBusy
		}
		session.State = "protected"
		session.TerminalReason = ""
		session.ReservationID = ""
		session.LastProgressAt = time.Now().UTC()
		session.StopRequestedAt = time.Time{}
		session.MayContainUnanchored = true
		session.CancelPhase = ""
		session.RecoveryStage = ""
		session.RecoveryReason = ""
		return session, true, true, nil
	}
	if session.State == "protected" {
		return session, true, false, nil
	}
	if session.State != "released" || session.TerminalReason != "empty" && session.TerminalReason != "cancelled" && session.TerminalReason != "failed" && session.TerminalReason != "expired" {
		return protectionSessionRecord{}, false, false, errors.New("existing protection session is not safely reusable")
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return protectionSessionRecord{}, false, false, ErrProtectionBusy
	}
	owners, err := g.countProtectionOwnersLocked(ctx, session.OwnerSetID)
	if err != nil {
		return protectionSessionRecord{}, false, false, err
	}
	if owners != 0 {
		return protectionSessionRecord{}, false, false, errors.New("released protection session still has demand owners")
	}
	now := time.Now().UTC()
	session.State = "protected"
	session.TerminalReason = ""
	session.ReservationID = ""
	session.LastProgressAt = now
	session.StopRequestedAt = time.Time{}
	session.PendingOperations = 0
	session.UncertainRemoteResult = false
	session.MayContainUnanchored = false
	session.CancelPhase = ""
	session.RecoveryStage = ""
	session.RecoveryReason = ""
	if err := g.saveProtectionSession(ctx, &session); err != nil {
		g.failClosedLocked("protection_reactivation_unknown")
		return protectionSessionRecord{}, false, false, err
	}
	return session, true, false, nil
}

func (g *writeGate) saveProtectionSessionBounded(session protectionSessionRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return g.saveProtectionSession(ctx, &session)
}

func (g *writeGate) discardProtectionSession(session protectionSessionRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if session.version != "" {
		_ = g.store.DeleteIfMatch(ctx, protectionSessionKey(session.ID), session.version)
	}
}

func (g *writeGate) linkProtectionOwnerLocked(ctx context.Context, session protectionSessionRecord, demandID string) error {
	if !validGateID(demandID) {
		return errors.New("protection demand ID is invalid")
	}
	key := protectionOwnerKey(session.OwnerSetID, demandID)
	data, err := json.Marshal(struct {
		SchemaVersion int       `json:"schemaVersion"`
		OwnerSetID    string    `json:"ownerSetID"`
		DemandID      string    `json:"demandID"`
		CreatedAt     time.Time `json:"createdAt"`
	}{1, session.OwnerSetID, demandID, time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = g.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) {
		return g.verifyProtectionOwner(ctx, key, session.OwnerSetID, demandID)
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		return g.verifyProtectionOwner(ctx, key, session.OwnerSetID, demandID)
	}
	return err
}

func (g *writeGate) verifyProtectionOwner(ctx context.Context, key, ownerSetID, demandID string) error {
	object, err := g.store.Get(ctx, key)
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || len(data) > 1<<20 {
		return errors.Join(readErr, closeErr, errors.New("protection owner could not be read safely"))
	}
	var owner struct {
		SchemaVersion int    `json:"schemaVersion"`
		OwnerSetID    string `json:"ownerSetID"`
		DemandID      string `json:"demandID"`
	}
	if err := json.Unmarshal(data, &owner); err != nil || owner.SchemaVersion != 1 || owner.OwnerSetID != ownerSetID || owner.DemandID != demandID {
		return errors.New("protection owner record conflicts with the requested identity")
	}
	return nil
}

func (g *writeGate) countProtectionOwnersLocked(ctx context.Context, ownerSetID string) (int, error) {
	count := 0
	token := ""
	for {
		entries, next, err := g.store.List(ctx, "system/solidification/owners/"+ownerSetID+"/", token, 1000)
		if err != nil {
			return 0, err
		}
		for _, entry := range entries {
			ownerID := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "system/solidification/owners/"+ownerSetID+"/"), ".json")
			if !validGateID(ownerID) {
				return 0, errors.New("protection owner index contains an invalid demand ID")
			}
			count++
		}
		if next == "" {
			break
		}
		token = next
	}
	return count, nil
}

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
		return protectionSessionStatus{}, storage.ErrNotFound
	}
	return g.protectionSessionStatusLocked(ctx, sessionID)
}

func (g *writeGate) protectionSessionStatusLocked(ctx context.Context, sessionID string) (protectionSessionStatus, error) {
	session, err := g.readProtectionSession(ctx, sessionID)
	if err != nil {
		return protectionSessionStatus{}, err
	}
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
