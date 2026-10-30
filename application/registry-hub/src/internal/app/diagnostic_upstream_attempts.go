package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
)

func (s *Service) recordUpstreamHTTPAttempt(ctx context.Context, attempt upstream.HTTPAttempt) {
	duration := attempt.FinishedAt.Sub(attempt.StartedAt).Seconds()
	event := diagnostics.Event{Component: "upstream", Operation: attempt.Method, OperationID: attempt.OperationID, AttemptID: attempt.ID, Attempt: attempt.Number, AttemptLayer: attempt.Layer, Stage: attempt.Stage, StartedAt: attempt.StartedAt, FinishedAt: attempt.FinishedAt, Authority: attempt.Authority, Repository: attempt.Repository, Digest: attempt.Digest, HTTPStatus: attempt.Status, ExecutionDurationSeconds: &duration, ExpectedBytes: attempt.ExpectedBytes, Result: "response_headers_received", TimeoutScope: "unknown", Responsibility: "unknown"}
	if attempt.Stage == "response_body" {
		event.TransportBytes, event.BodyComplete = &attempt.TransportBytes, &attempt.BodyComplete
		event.Result = "body_closed_before_eof"
		if attempt.BodyComplete {
			event.Result = "body_consumed"
		}
	}
	if attempt.Error != nil {
		event.Result, event.ErrorClass, event.ErrorSummary = "failed", diagnosticReason(attempt.Error), diagnostics.SafeErrorText(attempt.Error)
		event.Retryable = upstream.Retryable(attempt.Error)
		if ctx.Err() != nil {
			event.TimeoutScope = "enclosing_context"
		} else if event.ErrorClass == "timeout" {
			event.TimeoutScope = "upstream_transport"
		}
	} else if attempt.Status >= 400 {
		event.Result, event.ErrorClass, event.ErrorSummary = "http_response", "http_response", "upstream returned a non-success HTTP status"
		event.Retryable = upstream.RetryableStatus(attempt.Status)
	}
	if background, ok := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		event.WorkKind, event.WorkID, event.RunID, event.ParentAttemptID = background.Kind, background.WorkID, background.RunID, background.AttemptID
		if ctx.Err() != nil {
			event.TimeoutScope = background.Scope
		}
	}
	if task, ok := ctx.Value(diagnosticTaskKey{}).(*tasks.Task); ok {
		event.TaskID, event.ParentAttemptID = task.ID, task.AttemptID
		if task.Kind == tasks.RootKind {
			event.ImageID = inventoryIdentity(task.Authority, task.Repository, task.Reference).ID()
		} else {
			event.IntentID = task.ID
		}
	}
	if meta, ok := ctx.Value(requestMetaKey{}).(*requestMeta); ok {
		event.RequestID = meta.requestID
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := max(0, deadline.Sub(attempt.StartedAt).Seconds())
		event.DeadlineRemainingSeconds = &remaining
	}
	s.emitDiagnosticEvent(event)
}
