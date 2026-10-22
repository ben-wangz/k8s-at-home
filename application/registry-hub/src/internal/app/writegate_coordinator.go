package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"sort"
	"strings"
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
