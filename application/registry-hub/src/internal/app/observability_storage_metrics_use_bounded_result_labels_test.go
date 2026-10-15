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
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestStorageMetricsUseBoundedResultLabels(t *testing.T) {
	metrics := newMetrics()
	results := []struct {
		err   error
		label string
	}{
		{err: nil, label: "success"},
		{err: storage.ErrNotFound, label: "not_found"},
		{err: storage.ErrAlreadyExists, label: "already_exists"},
		{err: storage.ErrConditionFailed, label: "condition_failed"},
		{err: storage.ErrTimeout, label: "timeout"},
		{err: storage.ErrUnavailable, label: "unavailable"},
		{err: storage.ErrPermissionDenied, label: "permission_denied"},
		{err: storage.ErrCorruptObject, label: "corrupt"},
		{err: storage.ErrUnknownCommit, label: "unknown_commit"},
		{err: storage.ErrLeaseLost, label: "lease_lost"},
		{err: storage.ErrInvalidKey, label: "invalid_key"},
		{err: context.Canceled, label: "cancelled"},
		{err: errors.New("repository=private digest=private token=private"), label: "error"},
	}
	for _, result := range results {
		metrics.recordStorage("put", result.err, time.Millisecond)
	}
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, result := range results {
		if !strings.Contains(response.Body.String(), `operation="put",result="`+result.label+`"`) {
			t.Errorf("storage metrics omit bounded result label %q", result.label)
		}
	}
	for _, sensitive := range []string{"repository=private", "digest=private", "token=private"} {
		if strings.Contains(response.Body.String(), sensitive) {
			t.Errorf("storage metrics exposed high-cardinality data %q", sensitive)
		}
	}
}

func TestFeatureMetricsUseBoundedLabelsAndExposeRequiredFamilies(t *testing.T) {
	metrics := newMetrics()
	metrics.recordRequest("manifests", http.MethodGet, http.StatusBadGateway, "upstream", "digest_mismatch", time.Millisecond)
	metrics.recordRequest("manifests", http.MethodGet, http.StatusNotAcceptable, "upstream", "manifest_negotiation", time.Millisecond)
	metrics.recordRequest("blobs", http.MethodGet, http.StatusOK, "local", "", time.Millisecond)
	metrics.recordEvent("upstream_auth_challenge")
	metrics.recordEvent("upstream_redirect")
	metrics.recordEvent("upstream_retry")
	metrics.recordEvent("digest_mismatch")
	metrics.recordEvent("lease_lost")
	metrics.recordEvent("download_wait_timeout")
	metrics.objectLeases.WithLabelValues("active").Inc()
	metrics.objectLeases.WithLabelValues("waiting").Inc()
	metrics.setCacheStats(artifact.CacheStats{Objects: 2, Bytes: 7, Staging: 1, Orphans: 1, CleanupPending: 1})
	metrics.recordDistributionError("503")
	metrics.recordDistributionError("token=private")
	metrics.recordDistributionValidation("referrers")
	metrics.recordDistributionValidation("digest=private")
	metrics.referrerSources.WithLabelValues("native").Inc()
	metrics.referrerSources.WithLabelValues("fallback").Inc()
	metrics.setReferrerRepairPending(true)
	metrics.recordReferrerReconcile("success")
	metrics.recordReferrerReconcile("private-repository")

	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	for _, sample := range []string{
		`registry_hub_requests_total{cache_source="upstream",error_class="digest_mismatch",method="GET",operation="manifests",status="Bad Gateway"} 1`,
		`registry_hub_requests_total{cache_source="upstream",error_class="manifest_negotiation",method="GET",operation="manifests",status="Not Acceptable"} 1`,
		`registry_hub_object_leases{state="active"} 1`,
		`registry_hub_cache_objects 2`,
		`registry_hub_cache_bytes 7`,
		`registry_hub_cache_staging_objects 1`,
		`registry_hub_cache_orphan_objects 1`,
		`registry_hub_cache_cleanup_pending_objects 1`,
		`registry_hub_distribution_api_errors_total{result="503"} 1`,
		`registry_hub_distribution_api_errors_total{result="other"} 1`,
		`registry_hub_distribution_response_validation_failures_total{class="referrers"} 1`,
		`registry_hub_distribution_response_validation_failures_total{class="other"} 1`,
		`registry_hub_referrer_reads_total{source="fallback"} 1`,
		`registry_hub_referrer_repair_pending 1`,
		`registry_hub_referrer_reconcile_total{result="failure"} 1`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("metrics omit sample %q", sample)
		}
	}
	for _, sensitive := range []string{"token=private", "digest=private", "private-repository"} {
		if strings.Contains(body, sensitive) {
			t.Errorf("metrics exposed unbounded or sensitive value %q", sensitive)
		}
	}
}

func TestClientAndAdminCredentialsAreIndependentAndNoAuthIsRateLimited(t *testing.T) {
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
	service := &Service{
		cfg: config.Config{
			ClientAuth: config.Auth{Basic: []config.Credential{{Username: "client", Password: "client-secret", Permissions: []string{"pull"}}}},
			AdminAuth:  config.Auth{Basic: []config.Credential{{Username: "admin", Password: "admin-secret", Permissions: []string{"task-admin"}}}},
			Limits:     config.Limits{RequestTimeout: time.Second},
		},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics: newMetrics(), tasks: manager,
		limiter: newRequestLimiter(100, 100, 100, 100), clientSlots: make(chan struct{}, 16), noAuthSlots: make(chan struct{}, 16),
	}
	requestClient := func(username, password string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v2", nil)
		request.SetBasicAuth(username, password)
		response := httptest.NewRecorder()
		service.handleClient(response, request)
		return response
	}
	requestAdmin := func(username, password string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
		request.SetBasicAuth(username, password)
		response := httptest.NewRecorder()
		service.handleAdmin(response, request)
		return response
	}
	if response := requestClient("client", "client-secret"); response.Code != http.StatusOK {
		t.Fatalf("client credential Registry request = %d, want 200", response.Code)
	}
	if response := requestClient("admin", "admin-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("admin credential Registry request = %d, want 401", response.Code)
	}
	if response := requestAdmin("client", "client-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("client credential management request = %d, want 401", response.Code)
	}
	if response := requestAdmin("admin", "admin-secret"); response.Code != http.StatusOK {
		t.Fatalf("admin credential management request = %d, want 200: %s", response.Code, response.Body.String())
	}
	noAuth := &Service{
		cfg:    config.Config{Limits: config.Limits{RequestTimeout: time.Second}},
		logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), metrics: newMetrics(),
		limiter: newRequestLimiter(100, 100, 0, 1), clientSlots: make(chan struct{}, 16), noAuthSlots: make(chan struct{}, 16),
	}
	for index, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		response := httptest.NewRecorder()
		noAuth.handleClient(response, httptest.NewRequest(http.MethodGet, "/v2", nil))
		if response.Code != want {
			t.Fatalf("no-auth Registry request %d = %d, want %d", index+1, response.Code, want)
		}
	}
}

func TestSolidifierLogsFencingFailureWithoutErrorDetails(t *testing.T) {
	ctx := context.Background()
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
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`)
	digest := artifact.CalculateSHA256(body)
	if err := manager.SetDiscovered(ctx, &task, digest, ociManifestMediaType); err != nil {
		t.Fatal(err)
	}
	cache := artifact.NewCache(store)
	if err := cache.Put(ctx, task.Authority, task.Repository, digest, ociManifestMediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	staleLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "old-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, staleLease); err != nil {
		t.Fatal(err)
	}
	currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "new-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, currentLease)

	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	service := &Service{store: store, cache: cache, tasks: manager, metrics: newMetrics(), logger: logger, solidificationSlots: make(chan struct{}, 1)}
	cfg := config.Config{Limits: config.Limits{RequestTimeout: time.Second, ManifestBytes: 1 << 20, MaxDescriptors: 10, DescriptorDataBytes: 1 << 20, ClosureDepth: 10, ClosureObjects: 10, ClosureBytes: 1 << 20}}
	processor := &solidifier{service: service, cfg: cfg, cache: cache, store: store, tasks: manager}
	err = processor.Process(ctx, &task, staleLease)
	if !errors.Is(err, lease.ErrLost) {
		t.Fatalf("Process() error = %v, want lost fencing token", err)
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode structured log: %v", err)
	}
	if record["error_class"] != "fencing" || record["task_id"] != task.ID {
		t.Fatalf("fencing log = %#v", record)
	}
	if strings.Contains(output.String(), digest) {
		t.Fatalf("fencing log exposed a content digest: %s", output.String())
	}
}
