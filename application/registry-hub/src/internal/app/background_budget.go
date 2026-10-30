package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"sync"
	"time"
)

type workerStatus struct {
	Phase                    string               `json:"phase,omitempty"`
	Resumability             string               `json:"resumability,omitempty"`
	LastEntityProgressAt     time.Time            `json:"lastEntityProgressAt"`
	ErrorClass               string               `json:"errorClass,omitempty"`
	LastError                *diagnostics.Failure `json:"lastError,omitempty"`
	RecoveredAt              time.Time            `json:"recoveredAt"`
	ID                       string               `json:"id"`
	Scope                    string               `json:"scope"`
	State                    string               `json:"state"`
	RunID                    string               `json:"runID"`
	LastHeartbeatAt          time.Time            `json:"lastHeartbeatAt"`
	LastStartedAt            time.Time            `json:"lastStartedAt"`
	LastFinishedAt           time.Time            `json:"lastFinishedAt"`
	AdmissionWaitSeconds     float64              `json:"admissionWaitSeconds"`
	ExecutionDurationSeconds float64              `json:"executionDurationSeconds"`
	ExecutionBudgetSeconds   float64              `json:"executionBudgetSeconds"`
	DeadlineRemainingSeconds float64              `json:"deadlineRemainingSeconds"`
	AdmissionYields          uint64               `json:"admissionYields"`
}

type backgroundParentKey struct{}

func (s *Service) updateWorker(kind string, update func(*workerStatus)) {
	s.workerMu.Lock()
	defer s.workerMu.Unlock()
	if s.workerStates == nil {
		s.workerStates = map[string]workerStatus{}
	}
	state := s.workerStates[kind]
	state.ID, state.Scope = kind, "process"
	state.LastHeartbeatAt = time.Now().UTC()
	update(&state)
	s.workerStates[kind] = state
}

func (s *Service) beginBackgroundUnit(parent context.Context, kind string, budget time.Duration) (context.Context, func(), bool) {
	started := time.Now()
	runID := requestID()
	s.updateWorker(kind, func(state *workerStatus) {
		state.State, state.RunID = "admission", runID
		state.ExecutionBudgetSeconds = budget.Seconds()
	})
	admission, cancelAdmission := context.WithTimeout(parent, 15*time.Second)
	release, admitted := s.backgroundBudget(admission)
	cancelAdmission()
	wait := time.Since(started)
	if !admitted || parent.Err() != nil {
		if s.metrics != nil && s.metrics.backgroundAdmission != nil {
			s.metrics.backgroundAdmission.WithLabelValues(kind, "yield").Observe(wait.Seconds())
		}
		if admitted {
			release()
		}
		s.updateWorker(kind, func(state *workerStatus) {
			state.State, state.AdmissionWaitSeconds = "budget_yield", wait.Seconds()
			state.AdmissionYields++
		})
		s.backgroundEvent(kind, "admission_yield", nil)
		seconds := wait.Seconds()
		s.emitDiagnosticEvent(diagnostics.Event{Component: "background", Operation: kind, WorkKind: kind, RunID: runID, Stage: "admission", Result: "budget_yield", Reason: "admission_budget_exhausted", TimeoutScope: "admission", AdmissionWaitSeconds: &seconds})
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	ctx = context.WithValue(ctx, backgroundParentKey{}, parent)
	scope := "execution"
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < budget {
		scope = "external_deadline"
	}
	ctx = context.WithValue(ctx, diagnosticContextKey{}, backgroundDiagnosticContext{Kind: kind, RunID: runID, Scope: scope, Budget: budget.Seconds(), Admission: wait.Seconds()})
	executing := time.Now()
	deadline, _ := ctx.Deadline()
	if s.metrics != nil && s.metrics.backgroundAdmission != nil {
		s.metrics.backgroundAdmission.WithLabelValues(kind, "admitted").Observe(wait.Seconds())
		s.metrics.backgroundRemaining.WithLabelValues(kind).Observe(max(0, time.Until(deadline).Seconds()))
	}
	s.updateWorker(kind, func(state *workerStatus) {
		state.State, state.LastStartedAt = "execution", executing.UTC()
		state.AdmissionWaitSeconds, state.DeadlineRemainingSeconds = wait.Seconds(), max(0, time.Until(deadline).Seconds())
	})
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			cancel()
			release()
			s.updateWorker(kind, func(state *workerStatus) {
				state.State, state.LastFinishedAt = "idle", time.Now().UTC()
				state.ExecutionDurationSeconds = time.Since(executing).Seconds()
			})
		})
	}, true
}

func (s *Service) backgroundCheckpoint(parent context.Context, kind string) (context.Context, context.CancelFunc) {
	s.updateWorker(kind, func(state *workerStatus) { state.State = "checkpoint" })
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	s.workerMu.Lock()
	runID := s.workerStates[kind].RunID
	s.workerMu.Unlock()
	return context.WithValue(ctx, diagnosticContextKey{}, backgroundDiagnosticContext{Kind: kind, RunID: runID, Scope: "checkpoint", Budget: 5}), cancel
}
