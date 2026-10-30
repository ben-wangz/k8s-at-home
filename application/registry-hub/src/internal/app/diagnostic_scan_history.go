package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"time"
)

func (s *Service) recordScanFailure(ctx context.Context, kind, stage string, status *backgroundScanStatus, outbox *[]diagnostics.Event, cause error, attempt uint64) {
	identity, _ := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext)
	now := time.Now().UTC()
	diagnostics.ObserveFailure(&status.LastError, &status.RecoveredAt, diagnosticReason(cause), diagnostics.SafeErrorText(cause), stage, identity.RunID, now, false)
	event := diagnostics.Event{Component: "background", Operation: kind, WorkKind: kind, RunID: identity.RunID, AttemptID: identity.RunID, AttemptLayer: "worker", Stage: stage, Result: "failed", EntityID: inventory.ID("source-entity", status.ErrorEntity), Attempt: attempt, ErrorClass: diagnosticReason(cause), ErrorSummary: diagnostics.SafeErrorText(cause), Retryable: true, NextAttemptAt: status.NextAttemptAt, ExecutionBudgetSeconds: &identity.Budget, AdmissionWaitSeconds: &identity.Admission}
	if ctx.Err() != nil {
		event.TimeoutScope = identity.Scope
	}
	diagnostics.Record(outbox, event)
}

func recordScanRecovery(kind, stage string, status *backgroundScanStatus, outbox *[]diagnostics.Event, entity string) {
	if entity == "" && (status.LastError == nil || !status.RecoveredAt.Before(status.LastError.OccurredAt)) {
		return
	}
	now := time.Now().UTC()
	parent := ""
	if status.LastError != nil && (entity == "" || entity == status.ErrorEntity) {
		parent = status.LastError.AttemptID
		diagnostics.ObserveFailure(&status.LastError, &status.RecoveredAt, "", "", stage, "", now, true)
		status.ErrorRecovered = true
	}
	if entity == "" {
		entity = status.ErrorEntity
	}
	diagnostics.Record(outbox, diagnostics.Event{Component: "background", Operation: kind, WorkKind: kind, RunID: status.RunID, ParentAttemptID: parent, AttemptLayer: "worker", Stage: stage, Result: "recovered", EntityID: inventory.ID("source-entity", entity), RecoveredAt: now})
}
