package app

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"sync/atomic"
	"time"
)

type uploadRequestReader struct {
	io.Reader
	bytes atomic.Int64
}

func (r *uploadRequestReader) Read(buffer []byte) (int, error) {
	n, err := r.Reader.Read(buffer)
	r.bytes.Add(int64(n))
	return n, err
}

func (u *uploadSessions) proxyUploadAttempt(ctx context.Context, request *http.Request, state uploadSession, query url.Values) (*http.Response, error) {
	if u.attemptObserver == nil {
		return u.registry.Do(ctx, request.Method, state.InternalPath, query, requestHeaders(request.Header), request.Body, request.ContentLength)
	}
	started := time.Now().UTC()
	reader := &uploadRequestReader{Reader: request.Body}
	var body io.Reader
	if request.Body != nil {
		body = reader
	}
	response, err := u.registry.Do(ctx, request.Method, state.InternalPath, query, requestHeaders(request.Header), body, request.ContentLength)
	finished, bytes := time.Now().UTC(), reader.bytes.Load()
	duration := finished.Sub(started).Seconds()
	id := inventory.ID("client_upload", "", state.Repository, "", state.ID)
	event := diagnostics.Event{Component: "client_upload", Operation: request.Method, Stage: "distribution_response_headers", Result: "response_headers_received", AttemptID: requestID(), AttemptLayer: "client_upload_http_attempt", Repository: state.Repository, EntityID: state.ID, TransferID: id, StartedAt: started, FinishedAt: finished, TransportBytes: &bytes, ExecutionDurationSeconds: &duration, Reason: "transport_body_bytes_are_not_remote_commit_evidence", Responsibility: "unknown", TimeoutScope: "unknown"}
	if request.ContentLength >= 0 {
		size := request.ContentLength
		event.ExpectedBytes = &size
	}
	if response != nil {
		event.HTTPStatus = response.StatusCode
	}
	if err != nil {
		event.Result, event.ErrorClass, event.ErrorSummary = "failed", diagnosticReason(err), diagnostics.SafeErrorText(err)
		if ctx.Err() != nil {
			event.TimeoutScope = "enclosing_context"
		}
	} else if response.StatusCode >= 400 {
		event.Result, event.ErrorClass, event.ErrorSummary = "http_response", "http_response", "Distribution returned a non-success upload response"
	}
	if meta, ok := ctx.Value(requestMetaKey{}).(*requestMeta); ok {
		event.RequestID = meta.requestID
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := max(0, deadline.Sub(started).Seconds())
		event.DeadlineRemainingSeconds = &remaining
	}
	u.attemptObserver(event)
	return response, err
}
