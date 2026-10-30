package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

type diagnosticTaskKey struct{}

func (s *Service) recordStorageHTTPAttempt(ctx context.Context, attempt storage.HTTPAttempt) {
	if ctx.Value(diagnosticWriteKey{}) != nil || ctx.Value(diagnosticReadKey{}) != nil {
		return
	}
	// Successful first requests are aggregated in metrics; retain failed and retried HTTP attempts.
	if attempt.Error == nil && attempt.Status < 400 && attempt.Number <= 1 {
		return
	}
	if attempt.Error == nil && (attempt.Status == 404 || attempt.Status == 409 || attempt.Status == 412) && attempt.Number <= 1 {
		return
	}
	duration := attempt.FinishedAt.Sub(attempt.StartedAt).Seconds()
	event := diagnostics.Event{Component: "storage", Operation: attempt.Method, Stage: "http_response_headers", Result: "failed", Attempt: attempt.Number, AttemptLayer: "s3_http_attempt", StartedAt: attempt.StartedAt, FinishedAt: attempt.FinishedAt, ExecutionDurationSeconds: &duration, HTTPStatus: attempt.Status, S3RequestID: attempt.RequestID, TimeoutScope: "storage_call", Responsibility: "unknown"}
	if attempt.Error != nil {
		event.ErrorClass, event.ErrorSummary = diagnosticReason(attempt.Error), diagnostics.SafeErrorText(attempt.Error)
	} else if attempt.Status >= 400 {
		event.ErrorClass, event.ErrorSummary = "http_response", "S3 returned a non-success response; see HTTP status"
	} else {
		event.Result = "response_headers_received_after_retry"
	}
	if background, ok := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		event.WorkKind, event.WorkID, event.RunID, event.ParentAttemptID = background.Kind, background.WorkID, background.RunID, background.AttemptID
		if ctx.Err() != nil {
			event.TimeoutScope = background.Scope
		}
	}
	if task, ok := ctx.Value(diagnosticTaskKey{}).(*tasks.Task); ok {
		event.TaskID, event.ParentAttemptID = task.ID, task.AttemptID
		event.Authority, event.Repository = task.Authority, task.Repository
		event.Digest = task.BlobDigest
		if task.Kind == tasks.RootKind {
			event.Digest = task.RootDigest
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
	event.AttemptID = inventory.ID("s3_http_attempt", event.ParentAttemptID, attempt.RequestID, attempt.StartedAt.String())
	s.emitDiagnosticEvent(event)
}

func (s *Service) initializeStorageAttemptObserver(store storage.Store) {
	if observed, ok := store.(*observedStore); ok {
		store = observed.Store
	}
	if observable, ok := store.(interface {
		SetHTTPAttemptObserver(storage.HTTPAttemptObserver)
	}); ok {
		observable.SetHTTPAttemptObserver(s.recordStorageHTTPAttempt)
	}
}
