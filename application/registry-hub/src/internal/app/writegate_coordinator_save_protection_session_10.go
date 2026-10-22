package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

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
