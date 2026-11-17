package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const runtimeProtectionReconcileSteps = 32

func (s *Service) runRuntimeProtectionReconciliation(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if !s.recoveryStarting.Load() && !s.gate.State().RecoveryBarrier {
			bounded, cancel := context.WithTimeout(ctx, 45*time.Second)
			s.reconcileRuntimeProtectionWrites(bounded)
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) signalTaskReconciliation() {
	s.signalBackgroundScan("task_reconciliation")
}

func (s *Service) reconcileRuntimeProtectionWrites(ctx context.Context) {
	token := ""
	for ctx.Err() == nil {
		sessions, next, err := s.gate.ListProtectionSessions(ctx, token, 100)
		if err != nil {
			s.backgroundEvent("protection_reconciliation", "session_scan_failed", err)
			return
		}
		for _, status := range sessions {
			if !status.UncertainRemoteResult || status.PendingOperations != 0 || time.Now().Before(status.RemoteRecoveryNextAttemptAt) {
				continue
			}
			if err := s.reconcileRuntimeProtectionWrite(ctx, status.ID); err != nil && !errors.Is(err, lease.ErrHeld) && !errors.Is(err, storage.ErrConditionFailed) {
				s.backgroundEvent("protection_reconciliation", "session_deferred", err)
			}
		}
		if next == "" {
			return
		}
		token = next
	}
}

func (s *Service) reconcileRuntimeProtectionWrite(ctx context.Context, sessionID string) error {
	manager := lease.NewManager(s.store)
	owner := s.instanceID
	if owner == "" {
		owner = requestID()
	}
	leaseCtx, _, release, err := acquireRenewedLease(ctx, manager, "protection-runtime-reconciliation/"+sessionID, owner+"-"+requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = release() }()

	session, active, err := s.gate.protectionSession(leaseCtx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || !session.UncertainRemoteResult || session.RemoteWrite == nil {
		return nil
	}
	if session.RemoteWriteAttemptID == "" || session.RemoteRecovery == nil {
		if err := s.gate.EnsureProtectionRemoteRecovery(leaseCtx, sessionID, *session.RemoteWrite); err != nil {
			return err
		}
		session, active, err = s.gate.protectionSession(leaseCtx, sessionID)
		if err != nil || !active {
			return err
		}
	}
	if session.RemoteRecovery == nil || session.RemoteRecovery.Write == nil || session.RemoteWrite == nil || *session.RemoteRecovery.Write != *session.RemoteWrite {
		return storage.ErrCorruptObject
	}
	checkpoint := *session.RemoteRecovery
	attemptID := session.RemoteWriteAttemptID
	recovery := &protectionRecovery{service: s}
	for step := 0; step < runtimeProtectionReconcileSteps; step++ {
		if checkpoint.Verified || checkpoint.Absent {
			if err := s.gate.MarkRecoveredProtectionContentObserved(leaseCtx, sessionID); err != nil {
				return err
			}
			if err := s.gate.MarkProtectionRuntimeWriteResolved(leaseCtx, sessionID, attemptID, *checkpoint.Write); err != nil {
				return err
			}
			s.signalTaskReconciliation()
			return nil
		}
		recovery.checkpoint.Remote = checkpoint
		if err := recovery.verifyRemoteStep(leaseCtx); err != nil {
			class := runtimeProtectionRecoveryErrorClass(err)
			_ = s.gate.RecordProtectionRuntimeRecoveryFailure(leaseCtx, sessionID, attemptID, class, time.Now().UTC())
			return err
		}
		checkpoint = recovery.checkpoint.Remote
		checkpoint.Attempts, checkpoint.NextAttemptAt, checkpoint.ErrorClass = 0, time.Time{}, ""
		if err := s.gate.SaveProtectionRemoteRecovery(leaseCtx, sessionID, attemptID, checkpoint); err != nil {
			return err
		}
	}
	return nil
}

func runtimeProtectionRecoveryErrorClass(err error) string {
	if errors.Is(err, ErrRecoveryBarrier) || errors.Is(err, storage.ErrCorruptObject) {
		return "remote_state_untrusted"
	}
	if errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
		return "reconciliation_unavailable"
	}
	return "distribution_reconcile"
}
