package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"time"
)

type diagnosticContextKey struct{}
type diagnosticWriteKey struct{}
type diagnosticReadKey struct{}
type backgroundDiagnosticContext struct {
	Kind, RunID, Scope string
	WorkID, AttemptID  string
	Budget, Admission  float64
}

func (s *Service) emitDiagnosticEvent(event diagnostics.Event) {
	if s.diagnosticEvents == nil {
		return
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.ID == "" {
		event.ID = diagnostics.ID(event.Component, event.Operation, event.OccurredAt.Format(time.RFC3339Nano), requestID())
	}
	if event.Responsibility == "" {
		event.Responsibility = "unknown"
	}
	if event.TimeoutScope == "" {
		event.TimeoutScope = "unknown"
	}
	if event.AttemptLayer == "" {
		event.AttemptLayer = "process_observation"
	}
	select {
	case s.diagnosticEvents <- event:
	default:
		s.diagnosticEventsDropped.Add(1)
	}
}

func (s *Service) recordDiagnosticStorageFailure(ctx context.Context, operation string, cause error, started time.Time) {
	if ctx.Value(diagnosticWriteKey{}) != nil || ctx.Value(diagnosticReadKey{}) != nil {
		return
	}
	duration := time.Since(started).Seconds()
	event := diagnostics.Event{Component: "storage", Operation: operation, Stage: "storage_call", Result: "failed", OccurredAt: started.UTC(), ErrorClass: diagnosticReason(cause), Reason: "storage_operation_failed", ExecutionDurationSeconds: &duration, TimeoutScope: "storage_call", AttemptLayer: "storage_call"}
	if diagnostic, ok := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		event.WorkKind, event.RunID = diagnostic.Kind, diagnostic.RunID
		event.WorkID, event.AttemptID = diagnostic.WorkID, diagnostic.AttemptID
		event.AdmissionWaitSeconds = &diagnostic.Admission
		event.ExecutionBudgetSeconds = &diagnostic.Budget
		if ctx.Err() != nil {
			event.TimeoutScope = diagnostic.Scope
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := max(0, deadline.Sub(started).Seconds())
		event.DeadlineRemainingSeconds = &remaining
	}
	if errors.Is(cause, context.Canceled) {
		event.Reason = "context_cancelled"
	}
	if meta, ok := ctx.Value(requestMetaKey{}).(*requestMeta); ok {
		event.RequestID = meta.requestID
	}
	if id, ok := ctx.Value(storageOperationIDKey{}).(string); ok {
		event.OperationID = id
	}
	var awsRequest interface{ S3RequestID() string }
	if errors.As(cause, &awsRequest) {
		event.S3RequestID = awsRequest.S3RequestID()
	}
	s.emitDiagnosticEvent(event)
}

func (s *Service) runDiagnosticEventSink(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-s.diagnosticEvents:
			s.updateWorker("diagnostic_events", func(state *workerStatus) { state.State = "execution" })
			bounded, cancel := context.WithTimeout(context.WithValue(ctx, diagnosticWriteKey{}, true), 5*time.Second)
			if err := (diagnostics.Log{Store: s.store}).Append(bounded, event); err != nil {
				s.diagnosticEventsDropped.Add(1)
			}
			cancel()
			s.updateWorker("diagnostic_events", func(state *workerStatus) { state.State = "idle" })
		}
	}
}
