package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type recoveryHistory struct {
	LastError        *diagnostics.Failure `json:"lastError,omitempty"`
	RecoveredAt      time.Time            `json:"recoveredAt"`
	DiagnosticOutbox []diagnostics.Event  `json:"diagnosticOutbox,omitempty"`
}

func (s *Service) observeProtectionRecovery(ctx context.Context) func(error) {
	started := time.Now().UTC()
	key := diagnostics.Prefix + "workers/protection_recovery.json"
	readCtx, readCancel := context.WithTimeout(ctx, 5*time.Second)
	history, _, readErr := inventory.Read[recoveryHistory](readCtx, s.store, key)
	readCancel()
	if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
		s.backgroundEvent("protection_recovery", "history_read_failed", readErr)
	}
	s.updateWorker("protection_recovery", func(state *workerStatus) {
		state.State, state.Phase, state.Resumability = "execution", "begin", "full_rescan_on_restart"
		state.LastStartedAt, state.RunID = started, requestID()
		if readErr == nil {
			state.LastError, state.RecoveredAt = history.LastError, history.RecoveredAt
		}
		if deadline, ok := ctx.Deadline(); ok {
			state.ExecutionBudgetSeconds = max(0, deadline.Sub(started).Seconds())
		}
	})
	return func(err error) {
		var observed workerStatus
		s.updateWorker("protection_recovery", func(state *workerStatus) {
			state.State, state.LastFinishedAt = "idle", time.Now().UTC()
			state.ExecutionDurationSeconds = time.Since(started).Seconds()
			state.ErrorClass = ""
			if err != nil {
				state.State, state.ErrorClass = "failed", diagnosticReason(err)
			}
			diagnostics.ObserveFailure(&state.LastError, &state.RecoveredAt, state.ErrorClass, diagnostics.SafeErrorText(err), state.Phase, state.RunID, state.LastFinishedAt, err == nil)
			observed = *state
		})
		result := "done"
		if err != nil {
			result = "failed"
		}
		event := diagnostics.Event{Component: "background", Operation: "protection_recovery", WorkKind: "protection_recovery", RunID: observed.RunID, AttemptID: observed.RunID, AttemptLayer: "worker", Stage: observed.Phase, Result: result, ErrorClass: observed.ErrorClass, ErrorSummary: diagnostics.SafeErrorText(err), OccurredAt: observed.LastFinishedAt, StartedAt: started, FinishedAt: observed.LastFinishedAt, RecoveredAt: observed.RecoveredAt}
		if err == nil && observed.LastError != nil {
			event.ParentAttemptID = observed.LastError.AttemptID
		}
		parent, ok := ctx.Value(backgroundParentKey{}).(context.Context)
		if !ok {
			parent = ctx
		}
		bounded, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if saveErr := inventory.Update(bounded, s.store, key, func(value *recoveryHistory) error {
			if observed.LastError != nil && (value.LastError == nil || observed.LastError.OccurredAt.After(value.LastError.OccurredAt)) {
				value.LastError = observed.LastError
			}
			if observed.LastError != nil && value.LastError != nil && observed.LastError.AttemptID == value.LastError.AttemptID && observed.LastError.OccurredAt.Equal(value.LastError.OccurredAt) && observed.RecoveredAt.After(value.RecoveredAt) {
				value.RecoveredAt = observed.RecoveredAt
			}
			diagnostics.Record(&value.DiagnosticOutbox, event)
			return nil
		}); saveErr != nil {
			s.backgroundEvent("protection_recovery", "history_checkpoint_failed", saveErr)
		}
	}
}

func (s *Service) protectionRecoveryStage(phase string, progressed bool) {
	s.updateWorker("protection_recovery", func(state *workerStatus) {
		state.Phase = phase
		if progressed {
			state.LastEntityProgressAt = time.Now().UTC()
		}
	})
}
