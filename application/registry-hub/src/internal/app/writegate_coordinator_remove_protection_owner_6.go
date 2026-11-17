package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (g *writeGate) RemoveProtectionOwner(ctx context.Context, sessionID, demandID string) (int, error) {
	if !validGateID(sessionID) || !validGateID(demandID) {
		return 0, errors.New("protection session and demand IDs must be valid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.recoveryMutationAllowed(ctx); err != nil {
		return 0, err
	}
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
	if err := g.recoveryMutationAllowed(ctx); err != nil {
		return false, err
	}
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
		if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWritePermit != "" {
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
		session.RecoveryStage = ""
		session.RecoveryReason = ""
		return nil
	})
}

func (g *writeGate) MarkProtectionWriteStarted(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	return g.markProtectionWriteStarted(ctx, sessionID, "", remoteWrite, false)
}

func (g *writeGate) AcquireProtectionWritePermit(ctx context.Context, sessionID, permitID string) error {
	if !validGateID(permitID) {
		return errors.New("protection write permit ID is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		err := g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
			if session.State != "protected" && session.State != "anchoring" {
				return errors.New("protection session no longer admits remote writes")
			}
			if session.UncertainRemoteResult || session.RemoteWrite != nil {
				return ErrProtectionReconcilePending
			}
			if session.RemoteWritePermit != "" {
				return ErrProtectionBusy
			}
			session.RemoteWritePermit = permitID
			session.LastProgressAt = time.Now().UTC()
			return nil
		})
		if !errors.Is(err, storage.ErrConditionFailed) {
			return err
		}
	}
	return ErrProtectionBusy
}

func (g *writeGate) ReleaseProtectionWritePermit(ctx context.Context, sessionID, permitID string) error {
	if !validGateID(permitID) {
		return errors.New("protection write permit ID is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		err := g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
			if session.RemoteWritePermit == "" {
				return nil
			}
			if session.RemoteWritePermit != permitID || session.UncertainRemoteResult || session.RemoteWrite != nil {
				return ErrProtectionBusy
			}
			session.RemoteWritePermit = ""
			session.LastProgressAt = time.Now().UTC()
			return nil
		})
		if !errors.Is(err, storage.ErrConditionFailed) {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (g *writeGate) MarkProtectionWriteStartedWithPermit(ctx context.Context, sessionID, permitID string, remoteWrite protectionRemoteWrite) error {
	if !validGateID(permitID) {
		return errors.New("protection write permit ID is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		err := g.markProtectionWriteStarted(ctx, sessionID, permitID, remoteWrite, true)
		if !errors.Is(err, storage.ErrConditionFailed) {
			return err
		}
	}
	return ErrProtectionBusy
}

func (g *writeGate) markProtectionWriteStarted(ctx context.Context, sessionID, permitID string, remoteWrite protectionRemoteWrite, requirePermit bool) error {
	if !validProtectionRemoteWrite(remoteWrite) {
		return errors.New("protection remote write identity is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if session.State != "protected" && session.State != "anchoring" {
			return errors.New("protection session no longer admits remote writes")
		}
		if session.UncertainRemoteResult || session.RemoteWrite != nil {
			return ErrProtectionReconcilePending
		}
		if requirePermit && session.RemoteWritePermit != permitID || !requirePermit && session.RemoteWritePermit != "" {
			return ErrProtectionBusy
		}
		session.UncertainRemoteResult = true
		writeCopy := remoteWrite
		session.RemoteWrite = &writeCopy
		session.RemoteWritePermit = ""
		session.RemoteWriteAttemptID = hashID("protection-write-attempt", requestID())
		session.RemoteRecovery = &protectionRemoteCheckpoint{Write: &writeCopy}
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
		session.RemoteWritePermit = ""
		session.RemoteWriteAttemptID = ""
		session.RemoteRecovery = nil
		session.RecoveryStage = ""
		session.RecoveryReason = ""
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) SaveProtectionRemoteRecovery(ctx context.Context, sessionID, attemptID string, checkpoint protectionRemoteCheckpoint) error {
	if !validGateID(attemptID) || checkpoint.Write == nil || !validProtectionRemoteWrite(*checkpoint.Write) || checkpoint.Attempts < 0 {
		return errors.New("protection remote recovery checkpoint is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != *checkpoint.Write || session.RemoteWriteAttemptID != attemptID {
			return storage.ErrConditionFailed
		}
		copy := checkpoint
		write := *checkpoint.Write
		copy.Write = &write
		session.RemoteRecovery = &copy
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) EnsureProtectionRemoteRecovery(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	if !validProtectionRemoteWrite(remoteWrite) {
		return errors.New("protection remote write identity is invalid")
	}
	attemptID := hashID("protection-write-attempt", requestID())
	for attempt := 0; attempt < 16; attempt++ {
		err := g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
			if !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != remoteWrite {
				return storage.ErrConditionFailed
			}
			if session.RemoteWriteAttemptID == "" {
				session.RemoteWriteAttemptID = attemptID
			}
			if session.RemoteRecovery == nil {
				writeCopy := remoteWrite
				session.RemoteRecovery = &protectionRemoteCheckpoint{Write: &writeCopy}
			}
			return nil
		})
		if !errors.Is(err, storage.ErrConditionFailed) {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (g *writeGate) MarkProtectionRuntimeWriteResolved(ctx context.Context, sessionID, attemptID string, remoteWrite protectionRemoteWrite) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != remoteWrite || session.RemoteWriteAttemptID != attemptID {
			return storage.ErrConditionFailed
		}
		session.UncertainRemoteResult = false
		session.RemoteWrite = nil
		session.RemoteWritePermit = ""
		session.RemoteWriteAttemptID = ""
		session.RemoteRecovery = nil
		session.RecoveryStage = ""
		session.RecoveryReason = ""
		session.LastProgressAt = time.Now().UTC()
		return nil
	})
}

func (g *writeGate) RecordProtectionRuntimeRecoveryFailure(ctx context.Context, sessionID, attemptID, class string, now time.Time) error {
	if class == "" || len(class) > 64 || strings.ContainsAny(class, "\r\n\x00") {
		return errors.New("protection recovery failure class is invalid")
	}
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if !session.UncertainRemoteResult || session.RemoteWrite == nil || session.RemoteWriteAttemptID != attemptID || session.RemoteRecovery == nil {
			return storage.ErrConditionFailed
		}
		checkpoint := *session.RemoteRecovery
		checkpoint.Attempts++
		checkpoint.ErrorClass = class
		checkpoint.NextAttemptAt = now.Add(remoteRecoveryRetryDelay(checkpoint.Attempts))
		session.RemoteRecovery = &checkpoint
		session.RecoveryStage = "runtime_remote_reconcile"
		session.RecoveryReason = "remote_result_unknown"
		session.LastProgressAt = now
		return nil
	})
}

func remoteRecoveryRetryDelay(attempt int) time.Duration {
	return min(5*time.Minute, time.Second*time.Duration(1<<min(attempt, 8)))
}

func (g *writeGate) CompleteProtectionStartupRecovery(ctx context.Context, sessionID string, reconciledWrite *protectionRemoteWrite) error {
	return g.updateProtectionSession(ctx, sessionID, func(session *protectionSessionRecord) error {
		if reconciledWrite != nil {
			if !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != *reconciledWrite {
				return errors.New("protection remote write changed during startup recovery")
			}
			session.UncertainRemoteResult = false
			session.RemoteWrite = nil
		} else if session.UncertainRemoteResult || session.RemoteWrite != nil {
			return errors.New("protection session still has an unresolved remote write")
		}
		session.RemoteWritePermit = ""
		session.RemoteWriteAttemptID = ""
		session.RemoteRecovery = nil
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
