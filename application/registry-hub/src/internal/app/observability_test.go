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
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

type unavailableReadStore struct {
	storage.Store
}

func (s unavailableReadStore) Get(context.Context, string) (storage.Object, error) {
	return storage.Object{}, storage.ErrUnavailable
}

type failingObservationStore struct {
	storage.Store
	err error
}

func (s failingObservationStore) Put(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.Entry, error) {
	return storage.Entry{}, s.err
}

func TestClientAuthFailureIsLoggedAsStructuredJSON(t *testing.T) {
	var output bytes.Buffer
	service := &Service{
		cfg: config.Config{
			ClientAuth: config.Auth{Basic: []config.Credential{{Username: "tester", Password: "secret", Permissions: []string{"pull"}}}},
			Limits:     config.Limits{RequestTimeout: time.Second},
		},
		logger:      slog.New(slog.NewJSONHandler(&output, nil)),
		metrics:     newMetrics(),
		limiter:     newRequestLimiter(100, 100, 100, 100),
		clientSlots: make(chan struct{}, 16),
		noAuthSlots: make(chan struct{}, 16),
	}
	response := httptest.NewRecorder()
	service.handleClient(response, httptest.NewRequest(http.MethodGet, "/v2/", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /v2/ status = %d, want 401", response.Code)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode structured log: %v", err)
	}
	if record["error_class"] != "authentication" || record["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("authentication log = %#v", record)
	}
	if strings.Contains(output.String(), "secret") {
		t.Fatalf("authentication log exposed a credential: %s", output.String())
	}
}

func TestBlobStorageFailureWarnsAndDoesNotFallBackUpstream(t *testing.T) {
	internal := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
	}))
	defer internal.Close()
	endpoint, err := url.Parse(internal.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK}}}
	var output bytes.Buffer
	service := &Service{
		cfg: config.Config{
			ClientAuth: config.Auth{Basic: []config.Credential{{Username: "puller", Password: "secret", Permissions: []string{"pull"}}}},
			Limits:     config.Limits{RequestTimeout: time.Second},
		},
		logger: slog.New(slog.NewJSONHandler(&output, nil)), metrics: newMetrics(),
		limiter: newRequestLimiter(100, 100, 100, 100), clientSlots: make(chan struct{}, 16), noAuthSlots: make(chan struct{}, 16),
		registry: registry, cache: artifact.NewCache(unavailableReadStore{Store: store}), upstream: remote,
	}
	request := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/team/image/blobs/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil)
	request.SetBasicAuth("puller", "secret")
	response := httptest.NewRecorder()
	service.handleClient(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("blob storage failure response = %d, Retry-After %q; want 503 and 5", response.Code, response.Header().Get("Retry-After"))
	}
	if len(remote.calls) != 0 {
		t.Fatalf("storage failure triggered %d upstream requests, want 0", len(remote.calls))
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode structured log: %v", err)
	}
	if record["level"] != "WARN" || record["error_class"] != "storage" {
		t.Fatalf("storage failure log = %#v, want WARN storage", record)
	}
}

func TestStorageFailureBlocksManifestPublishAndDelete(t *testing.T) {
	registryRequests := 0
	internal := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		registryRequests++
	}))
	defer internal.Close()
	endpoint, err := url.Parse(internal.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	gate, err := newWriteGate(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	service := &Service{
		cfg: config.Config{
			ClientAuth: config.Auth{Basic: []config.Credential{{Username: "writer", Password: "secret", Permissions: []string{"push", "delete"}}}},
			Limits:     config.Limits{RequestTimeout: time.Second, ManifestBytes: 1 << 20, MaxDescriptors: 100, DescriptorDataBytes: 1 << 20},
		},
		logger: slog.New(slog.NewJSONHandler(&output, nil)), metrics: newMetrics(),
		limiter: newRequestLimiter(100, 100, 100, 100), clientSlots: make(chan struct{}, 16), noAuthSlots: make(chan struct{}, 16),
		pushSlots: make(chan struct{}, 4), registry: registry, store: unavailableReadStore{Store: store}, tasks: manager, gate: gate,
	}
	manifest := []byte(ociManifestBody)
	request := httptest.NewRequest(http.MethodPut, "/v2/ghcr.io/team/image/manifests/latest", bytes.NewReader(manifest))
	request.Header.Set("Content-Type", ociManifestMediaType)
	request.SetBasicAuth("writer", "secret")
	response := httptest.NewRecorder()
	service.handleClient(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("manifest publish storage failure response = %d, Retry-After %q; want 503 and 5", response.Code, response.Header().Get("Retry-After"))
	}
	assertStorageWarning := func() {
		t.Helper()
		var record map[string]any
		if err := json.Unmarshal(output.Bytes(), &record); err != nil {
			t.Fatalf("decode structured log: %v", err)
		}
		if record["level"] != "WARN" || record["error_class"] != "storage" {
			t.Fatalf("storage failure log = %#v, want WARN storage", record)
		}
		output.Reset()
	}
	assertStorageWarning()

	request = httptest.NewRequest(http.MethodDelete, "/v2/ghcr.io/team/image/manifests/latest", nil)
	request.SetBasicAuth("writer", "secret")
	response = httptest.NewRecorder()
	service.handleClient(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "5" {
		t.Fatalf("manifest delete storage failure response = %d, Retry-After %q; want 503 and 5", response.Code, response.Header().Get("Retry-After"))
	}
	assertStorageWarning()
	if registryRequests != 0 {
		t.Fatalf("storage failure sent %d requests to internal Registry, want 0", registryRequests)
	}
}

func TestObservedStorageWarningRedactsBackendErrorDetails(t *testing.T) {
	var output bytes.Buffer
	metrics := newMetrics()
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	secretDetails := "https://bucket.example/object?X-Amz-Signature=signed-secret&X-Amz-Credential=private-key Authorization=Bearer private-token Cookie=session-private AWS_SECRET_ACCESS_KEY=private-secret"
	store := newObservedStore(failingObservationStore{err: errors.New(secretDetails)}, metrics, logger)
	_, err := store.Put(context.Background(), "repository/tag/sha256:private-digest", strings.NewReader("data"), 4, storage.PutOptions{})
	if err == nil {
		t.Fatal("Put succeeded despite the injected backend failure")
	}
	if strings.Contains(output.String(), secretDetails) || strings.Contains(output.String(), "private-digest") {
		t.Fatalf("storage warning exposed backend details: %s", output.String())
	}
	for _, sensitive := range []string{"signed-secret", "private-key", "private-token", "session-private", "private-secret"} {
		if strings.Contains(output.String(), sensitive) {
			t.Errorf("storage warning exposed %q: %s", sensitive, output.String())
		}
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode storage warning: %v", err)
	}
	if record["level"] != "WARN" || record["error_class"] != "error" || record["operation"] != "put" {
		t.Fatalf("storage warning = %#v, want a redacted put/error record", record)
	}
}
