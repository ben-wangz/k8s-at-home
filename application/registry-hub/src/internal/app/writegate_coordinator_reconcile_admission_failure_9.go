package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

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
