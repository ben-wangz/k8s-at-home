package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/tasks"
	"registry-hub/internal/upstream"
	"strings"
	"testing"
	"time"
)

func TestUpstreamAttemptDiagnosticKeepsParentAndSafeError(t *testing.T) {
	s := &Service{diagnosticEvents: make(chan diagnostics.Event, 2)}
	task := tasks.Task{ID: "task", AttemptID: "task-attempt", Kind: tasks.BlobKind}
	ctx := context.WithValue(context.Background(), diagnosticTaskKey{}, &task)
	now := time.Now().UTC()
	s.recordUpstreamHTTPAttempt(ctx, upstream.HTTPAttempt{ID: "http-attempt", OperationID: "operation", Layer: "token_http_attempt", Stage: "response_headers", Authority: "registry.example", Repository: "team/image", Number: 2, StartedAt: now, FinishedAt: now.Add(time.Second), Error: errors.New("https://private-token.example/?password=secret")})
	event := <-s.diagnosticEvents
	if event.ParentAttemptID != task.AttemptID || event.Attempt != 2 || event.AttemptLayer != "token_http_attempt" || event.ErrorSummary == "" {
		t.Fatalf("attempt=%+v", event)
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), "password") || strings.Contains(string(raw), "private-token") {
		t.Fatal("raw endpoint or credentials leaked")
	}
}

func TestClientUploadFailureHasIndependentHTTPAttempt(t *testing.T) {
	registry := newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	var event diagnostics.Event
	u := uploadSessions{registry: registry, attemptObserver: func(value diagnostics.Event) { event = value }}
	request := httptest.NewRequest(http.MethodPatch, "/v2/team/image/blobs/uploads/public", strings.NewReader("chunk"))
	state := uploadSession{ID: strings.Repeat("a", 32), Token: "private-token", Repository: "team/image", InternalPath: "/v2/internal/blobs/uploads/private"}
	response, err := u.proxyUploadAttempt(context.Background(), request, state, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if event.AttemptLayer != "client_upload_http_attempt" || event.HTTPStatus != 503 || event.TransportBytes == nil || *event.TransportBytes != 5 || event.TransferID != uploadTransferView(state).ID {
		t.Fatalf("attempt=%+v", event)
	}
	raw, _ := json.Marshal(event)
	if strings.Contains(string(raw), state.Token) || strings.Contains(string(raw), state.InternalPath) {
		t.Fatal("private upload state leaked")
	}
}
