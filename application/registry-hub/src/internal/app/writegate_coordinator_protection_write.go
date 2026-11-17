package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

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
