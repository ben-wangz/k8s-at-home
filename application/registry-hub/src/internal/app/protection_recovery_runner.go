package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) openProtectionRecovery(ctx context.Context) (*protectionRecovery, context.Context, func() error, error) {
	manager := lease.NewManager(s.store)
	owner := s.instanceID
	if owner == "" {
		owner = requestID()
	}
	heldCtx, held, release, err := acquireProtectionRecoveryLease(ctx, manager, owner)
	if err != nil {
		return nil, nil, nil, err
	}
	r := &protectionRecovery{service: s, manager: manager, lease: held}
	bootstrap := context.WithValue(heldCtx, recoveryWriteGuardKey{}, recoveryWriteGuard(func(ctx context.Context) error { return manager.Validate(ctx, held) }))
	if err := s.tasks.PauseForRecovery(heldCtx); err != nil {
		_ = release()
		return nil, nil, nil, err
	}
	if s.gate.recovery != nil {
		if err := s.gate.recovery.quiesce(heldCtx); err != nil {
			_ = release()
			return nil, nil, nil, err
		}
	}
	r.checkpoint, r.version, err = inventory.Read[protectionRecoveryCheckpoint](heldCtx, s.store, protectionRecoveryKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		_ = release()
		return nil, nil, nil, err
	}
	if err == nil && r.checkpoint.Schema != 1 {
		_ = release()
		return nil, nil, nil, storage.ErrCorruptObject
	}
	// Only this process can prove writes stayed frozen after an uncertain completion checkpoint.
	localCompletion := r.checkpoint.Phase == "complete" && r.checkpoint.Owner == s.instanceID && !s.cacheProtectionReady.Load()
	if r.checkpoint.Phase == "finalize" || localCompletion {
		state, readErr := readRecoveryGate(heldCtx, s.store)
		if readErr != nil {
			_ = release()
			return nil, nil, nil, readErr
		}
		if !state.RecoveryBarrier && state.RecoveryID == r.checkpoint.Generation && state.Epoch == r.checkpoint.Epoch && recoveryLedger(state) == r.checkpoint.ValidationLedger {
			s.gate.mu.Lock()
			err = s.gate.reloadLocked(heldCtx)
			s.gate.mu.Unlock()
			if err != nil {
				_ = release()
				return nil, nil, nil, err
			}
			r.checkpoint.Phase, r.checkpoint.Owner, r.checkpoint.Fence = "complete", owner, held.FencingToken
			guarded := context.WithValue(heldCtx, recoveryWriteGuardKey{}, recoveryWriteGuard(r.guard))
			return r, guarded, release, nil
		}
	}
	state, err := s.gate.claimProtectionRecovery(bootstrap, held.FencingToken)
	if err != nil {
		_ = release()
		return nil, nil, nil, err
	}
	if r.checkpoint.Generation != state.RecoveryID {
		r.checkpoint = protectionRecoveryCheckpoint{Schema: 1, Generation: state.RecoveryID, Epoch: state.Epoch, Phase: "quiesce", StartedAt: time.Now().UTC(), CacheReservations: map[string]int64{}, LegacyOwners: map[string]bool{}, LegacyRequired: map[string]bool{}}
	}
	if r.checkpoint.LegacyOwners == nil {
		r.checkpoint.LegacyOwners = map[string]bool{}
	}
	if r.checkpoint.LegacyRequired == nil {
		r.checkpoint.LegacyRequired = map[string]bool{}
	}
	if r.checkpoint.CacheReservations == nil {
		r.checkpoint.CacheReservations = map[string]int64{}
	}
	r.checkpoint.Trigger = state.RecoveryTrigger
	r.checkpoint.Owner, r.checkpoint.Fence = owner, held.FencingToken
	guarded := context.WithValue(heldCtx, recoveryWriteGuardKey{}, recoveryWriteGuard(r.guard))
	if err := r.save(guarded); err != nil {
		_ = release()
		return nil, nil, nil, err
	}
	return r, guarded, release, nil
}

func (s *Service) recoverProtectionQuantum(ctx context.Context) (result error) {
	s.protectionRecoveryMu.Lock()
	defer s.protectionRecoveryMu.Unlock()
	finished := s.observeProtectionRecovery(ctx)
	defer func() { finished(result) }()
	r, heldCtx, release, err := s.openProtectionRecovery(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			if errors.Is(result, errProtectionRecoveryYield) {
				result = releaseErr
			} else {
				result = errors.Join(result, releaseErr)
			}
		}
	}()
	s.cacheProtectionReady.Store(false)
	reserve := protectionRecoveryCheckpointBudget
	if deadline, ok := ctx.Deadline(); ok {
		reserve = min(reserve, time.Until(deadline)/4)
	}
	lastCheckpoint := time.Now()
	for units := 0; units < 128; units++ {
		budget := 30 * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			if time.Until(deadline) <= reserve {
				return r.yield(heldCtx)
			}
			budget = min(budget, time.Until(deadline)-reserve)
		}
		bounded, cancel := context.WithTimeout(heldCtx, budget)
		before, _ := json.Marshal(r.checkpoint)
		err := r.step(bounded)
		quantumExpired := budget < 30*time.Second && errors.Is(bounded.Err(), context.DeadlineExceeded)
		cancel()
		if err != nil {
			var previous protectionRecoveryCheckpoint
			if decodeErr := json.Unmarshal(before, &previous); decodeErr != nil {
				return decodeErr
			}
			r.checkpoint = previous
			if quantumExpired {
				return r.yield(heldCtx)
			}
			return r.failed(heldCtx, err)
		}
		r.checkpoint.Processed++
		r.checkpoint.ProgressAt = time.Now().UTC()
		r.checkpoint.Attempts, r.checkpoint.NextAttemptAt = 0, time.Time{}
		r.checkpoint.Reason = ""
		var previous protectionRecoveryCheckpoint
		_ = json.Unmarshal(before, &previous)
		if previous.Phase != r.checkpoint.Phase || units%8 == 7 || time.Since(lastCheckpoint) >= 2*time.Second || r.checkpoint.Phase == "complete" {
			if err := r.save(heldCtx); err != nil {
				return err
			}
			lastCheckpoint = time.Now()
		}
		if r.checkpoint.Phase == "complete" {
			if s.gate.recovery != nil {
				s.gate.recovery.blocked.Store(false)
			}
			s.cacheProtectionReady.Store(true)
			s.recoveryStarting.Store(false)
			s.tasks.ResumeAfterRecovery()
			s.refreshReadiness()
			return nil
		}
	}
	return r.yield(heldCtx)
}

func (r *protectionRecovery) yield(ctx context.Context) error {
	r.checkpoint.Yields++
	r.checkpoint.Reason = "execution_quantum_complete"
	if err := r.save(ctx); err != nil {
		return err
	}
	return errProtectionRecoveryYield
}

func (r *protectionRecovery) failed(ctx context.Context, cause error) error {
	r.checkpoint.Attempts++
	r.checkpoint.Reason = diagnosticReason(cause)
	r.checkpoint.NextAttemptAt = time.Now().UTC().Add(time.Second * time.Duration(1<<min(r.checkpoint.Attempts, 6)))
	diagnostics.ObserveFailure(&r.checkpoint.LastError, &r.checkpoint.RecoveredAt, diagnosticReason(cause), diagnostics.SafeErrorText(cause), r.checkpoint.Phase, r.checkpoint.Generation, time.Now().UTC(), false)
	return errors.Join(cause, r.save(ctx))
}

func (r *protectionRecovery) next(phase string) {
	r.checkpoint.Phase, r.checkpoint.Step, r.checkpoint.Entity = phase, "", ""
	r.checkpoint.Cursor, r.checkpoint.Inner = inventory.Cursor{}, inventory.Cursor{}
	r.checkpoint.Index, r.checkpoint.Half = 0, 0
}
