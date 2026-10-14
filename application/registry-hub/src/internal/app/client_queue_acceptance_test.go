package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/distribution"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func fillDiscoveryQueue(t *testing.T, manager *tasks.Manager) {
	t.Helper()
	for index := range 10 {
		if _, _, err := manager.Enqueue(context.Background(), "registry.example", fmt.Sprintf("team/image-%d", index), "latest"); err != nil {
			t.Fatalf("enqueue queue item %d: %v", index, err)
		}
	}
}

func taskCount(t *testing.T, manager *tasks.Manager) int {
	t.Helper()
	items, _, err := manager.List(context.Background(), "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(items)
}

func newLocalRegistry(t *testing.T, handler http.Handler) *distribution.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := distribution.New(config.Config{DistributionURL: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	return registry
}

func TestManifestQueueFullDoesNotRequestUpstream(t *testing.T) {
	remote := &scriptedUpstream{}
	service, manager, routed := newDiscoveryTestService(t, remote)
	fillDiscoveryQueue(t, manager)

	localRequests := 0
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		localRequests++
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"missing"}]}`)
	}))
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("queue-full response = %d Retry-After=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if localRequests == 0 {
		t.Fatal("request did not check the internal Registry before queue admission")
	}
	if len(remote.calls) != 0 {
		t.Fatalf("upstream calls = %#v, want none", remote.calls)
	}
	if got := taskCount(t, manager); got != 10 {
		t.Fatalf("task count = %d, want the original full queue of 10", got)
	}
}

type persistedTaskProbeUpstream struct {
	manager      *tasks.Manager
	calls        int
	sawPending   bool
	observedTask string
}

func (u *persistedTaskProbeUpstream) Do(ctx context.Context, _, _, _, _ string, _ url.Values, _ http.Header) (*http.Response, error) {
	u.calls++
	items, _, err := u.manager.List(ctx, "", "", 100)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.Authority == "ghcr.io" && item.Repository == "team/image" && item.Reference == "latest" {
			u.observedTask = item.ID
			u.sawPending = item.Status == tasks.Pending && item.Phase == "discovery"
			break
		}
	}
	return &http.Response{
		StatusCode: http.StatusInternalServerError,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"errors":[{"code":"UNKNOWN"}]}`)),
	}, nil
}

func (*persistedTaskProbeUpstream) Close() {}

func TestDiscoveryTaskIsPersistedBeforeUpstreamRequest(t *testing.T) {
	service, manager, routed := newDiscoveryTestService(t, &scriptedUpstream{})
	remote := &persistedTaskProbeUpstream{manager: manager}
	service.upstream = remote
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
	}))
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})

	if remote.calls != 1 || !remote.sawPending || remote.observedTask == "" {
		t.Fatalf("upstream observed calls=%d pending=%t task=%q; expected a persisted pending discovery task before fetch", remote.calls, remote.sawPending, remote.observedTask)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("upstream failure status = %d, want 500", recorder.Code)
	}
}

type unavailableWriteStore struct {
	storage.Store
}

func (s unavailableWriteStore) Put(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.Entry, error) {
	return storage.Entry{}, storage.ErrUnavailable
}

func TestDiscoveryReservationStorageFailureDoesNotRequestUpstream(t *testing.T) {
	baseStore, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer baseStore.Close()
	store := unavailableWriteStore{Store: baseStore}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: ociManifestBody, contentType: ociManifestMediaType}}}
	service := retryTestService(remote)
	service.cfg = config.Config{Limits: config.Limits{ManifestBytes: 1 << 20, MaxDescriptors: 100, DescriptorDataBytes: 1 << 20}}
	service.tasks = manager
	service.store = store
	service.cache = artifact.NewCache(store)
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
	}))
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})

	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("reservation failure response = %d Retry-After=%q body=%s, want 503 with Retry-After", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
	}
	if len(remote.calls) != 0 {
		t.Fatalf("reservation failure triggered %d upstream requests, want 0", len(remote.calls))
	}
	if got := taskCount(t, manager); got != 0 {
		t.Fatalf("failed reservation left %d persisted tasks, want 0", got)
	}
}

func TestLocalManifestHitBypassesFullSolidificationQueue(t *testing.T) {
	remote := &scriptedUpstream{}
	service, manager, routed := newDiscoveryTestService(t, remote)
	fillDiscoveryQueue(t, manager)
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", ociIndexMediaType)
		writer.Header().Set("Docker-Content-Digest", "sha256:dff9de10919148711140d349bf03f1a99eb06f94b03e51715ccebfa7cdc518e2")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer, ociIndexBody)
	}))
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociIndexMediaType)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})

	if recorder.Code != http.StatusOK || recorder.Body.String() != ociIndexBody {
		t.Fatalf("local hit response = %d %q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 0 {
		t.Fatalf("upstream calls = %#v, want none", remote.calls)
	}
	if got := taskCount(t, manager); got != 10 {
		t.Fatalf("task count = %d, want the original full queue of 10", got)
	}
}
