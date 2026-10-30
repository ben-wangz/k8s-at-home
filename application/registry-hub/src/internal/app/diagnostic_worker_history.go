package app

import (
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/lease"
	"time"
)

func (s *Service) recordWorkerObservation(kind, stage string, cause error) {
	if errors.Is(cause, lease.ErrHeld) {
		return
	}
	var state workerStatus
	s.updateWorker(kind, func(current *workerStatus) {
		diagnostics.ObserveFailure(&current.LastError, &current.RecoveredAt, diagnosticReason(cause), diagnostics.SafeErrorText(cause), stage, current.RunID, time.Now().UTC(), false)
		state = *current
	})
	event := diagnostics.Event{Component: "background", Operation: kind, WorkKind: kind, RunID: state.RunID, Stage: stage, Result: "deferred", ErrorClass: diagnosticReason(cause), ErrorSummary: diagnostics.SafeErrorText(cause), ExecutionBudgetSeconds: &state.ExecutionBudgetSeconds, AdmissionWaitSeconds: &state.AdmissionWaitSeconds}
	if event.ErrorClass == "timeout" || event.ErrorClass == "cancelled" {
		switch stage {
		case "lease_renewal_failed":
			event.TimeoutScope = "lease_renewal"
		case "lease_release_failed":
			event.TimeoutScope = "lease_release"
		case "lease_failed", "lease_acquisition_failed":
			event.TimeoutScope = "lease_acquisition"
		}
	}
	s.emitDiagnosticEvent(event)
}
