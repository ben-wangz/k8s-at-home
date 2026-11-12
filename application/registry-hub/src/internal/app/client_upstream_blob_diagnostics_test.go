package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

func TestUpstreamBlobFailureLogUsesBoundedRequestFields(t *testing.T) {
	const (
		requestID = "blob-request-123"
		secret    = "signed-url-token"
		digest    = "sha256:private-content-digest"
	)
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewJSONHandler(&output, nil))}
	err := withUpstreamBlobStage("cache_representation_commit", errors.Join(storage.ErrTimeout, errors.New(secret+digest)))
	meta := &requestMeta{
		requestID: requestID, operation: "blob", cacheSource: "upstream", errorClass: "storage",
	}
	recordUpstreamBlobFailure(meta, "cache_fill", err, 9*time.Minute)
	service.logRegistryRequest(meta, http.MethodGet, http.StatusServiceUnavailable, 9*time.Minute)

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("decode failure log: %v", err)
	}
	for key, want := range map[string]any{
		"msg":                "registry request",
		"request_id":         requestID,
		"operation":          "blob",
		"failure_stage":      "cache_representation_commit",
		"failure_cause":      "timeout",
		"error_class":        "storage",
		"failure_elapsed_ms": float64((9 * time.Minute).Milliseconds()),
	} {
		if got := record[key]; got != want {
			t.Errorf("log %s = %#v, want %#v", key, got, want)
		}
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), digest) {
		t.Fatalf("failure log leaked error detail or content identity: %s", output.String())
	}
}

func TestUpstreamBlobFailureStagePreservesDownloadPersistenceStage(t *testing.T) {
	err := withUpstreamBlobStage("cache_fill", &download.PersistenceError{Stage: "state_commit", Err: storage.ErrTimeout})
	if got := upstreamBlobFailureStage(err, "fallback"); got != "state_commit" {
		t.Fatalf("failure stage = %q, want state_commit", got)
	}
	if got := upstreamBlobFailureCause(err); got != "timeout" {
		t.Fatalf("failure class = %q, want timeout", got)
	}
	if !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("staged failure does not preserve its cause: %v", err)
	}
}

func TestUpstreamBlobFetchFailureLoggingSkipsExpectedClientStatuses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   bool
	}{
		{name: "network failure", status: 0, want: true},
		{name: "not found", status: http.StatusNotFound, want: false},
		{name: "unauthorized", status: http.StatusUnauthorized, want: false},
		{name: "server failure", status: http.StatusBadGateway, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldLogUpstreamBlobFetchFailure(&upstreamBlobFetchError{status: test.status}); got != test.want {
				t.Fatalf("shouldLogUpstreamBlobFetchFailure(status=%d) = %t, want %t", test.status, got, test.want)
			}
		})
	}
}

func TestFinalUpstreamBlobCacheLookupLogsFailureStage(t *testing.T) {
	const secret = "private-storage-detail"
	body := "upstream blob whose final cache lookup fails"
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: body, contentType: "application/octet-stream"}}}
	service, _, _ := newDiscoveryTestService(t, remote)
	store := &postCommitIndexFailureStore{Store: service.store, failure: errors.Join(storage.ErrUnavailable, errors.New(secret))}
	service.cache = artifact.NewCache(store)
	var output bytes.Buffer
	service.logger = slog.New(slog.NewJSONHandler(&output, nil))
	routed, err := route.Parse("/v2/ghcr.io/team/image/blobs/" + artifact.CalculateSHA256([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	meta := &requestMeta{requestID: "post-fill-cache-lookup"}
	response := httptest.NewRecorder()
	service.serveUpstreamBlob(response, httptest.NewRequest(http.MethodGet, routed.RawPath, nil), routed, meta)
	service.logRegistryRequest(meta, http.MethodGet, response.Code, 2*time.Second)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "verified blob cache is unavailable") || len(remote.calls) != 1 {
		t.Fatalf("final lookup response status=%d upstream_calls=%d body=%q", response.Code, len(remote.calls), response.Body.String())
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &record); err != nil {
		t.Fatalf("decode failure log: %v", err)
	}
	if record["msg"] != "registry request" || record["failure_stage"] != "cache_representation_index_read" || record["failure_cause"] != "unavailable" || record["error_class"] != "storage" || record["request_id"] != meta.requestID {
		t.Fatalf("unexpected failure log fields: %#v", record)
	}
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), routed.Reference) {
		t.Fatalf("failure log leaked storage detail or content digest: %s", output.String())
	}
}

type postCommitIndexFailureStore struct {
	storage.Store
	failure         error
	indexWasWritten bool
}

func (s *postCommitIndexFailureStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.indexWasWritten && strings.HasSuffix(key, "/representation-index.json") {
		s.indexWasWritten = false
		return storage.Object{}, s.failure
	}
	return s.Store.Get(ctx, key)
}

func (s *postCommitIndexFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && strings.HasSuffix(key, "/representation-index.json") {
		s.indexWasWritten = true
	}
	return entry, err
}
