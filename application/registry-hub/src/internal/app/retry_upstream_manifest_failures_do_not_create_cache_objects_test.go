package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/route"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestUpstreamManifestFailuresDoNotCreateCacheObjects(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
		{status: http.StatusServiceUnavailable, retryAfter: "0"},
	}}
	service := retryTestService(remote)
	service.cfg = config.Config{Limits: config.Limits{ManifestBytes: 1 << 20}}
	service.store = store
	service.cache = artifact.NewCache(store)
	service.tasks = manager
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json")
	recorder := httptest.NewRecorder()
	service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if len(remote.calls) != maxUpstreamRetryAttempts {
		t.Fatalf("upstream calls = %d, want %d", len(remote.calls), maxUpstreamRetryAttempts)
	}
	objects, _, err := store.List(context.Background(), "cache/objects/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 0 {
		t.Fatalf("upstream error created %d cache objects: %#v", len(objects), objects)
	}
}

func TestRetryDelayKeepsRetryAfterSeparateFromDefaultBudget(t *testing.T) {
	if delay, ok := upstreamRetryDelay("30", 0, maxUpstreamRetryWait); !ok || delay != 30*time.Second {
		t.Fatalf("Retry-After delay = %s, %t; want 30s, true", delay, ok)
	}
	if delay, ok := upstreamRetryDelay("120", 0, 0); !ok || delay != maxRetryAfterDelay {
		t.Fatalf("capped Retry-After delay = %s, %t; want %s, true", delay, ok, maxRetryAfterDelay)
	}
	if delay, ok := upstreamRetryDelay("invalid", 1, 14*time.Second); !ok || delay != time.Second {
		t.Fatalf("default remaining delay = %s, %t; want 1s, true", delay, ok)
	}
	if _, ok := upstreamRetryDelay("invalid", 1, maxUpstreamRetryWait); ok {
		t.Fatal("default backoff exceeded its total wait budget")
	}
}
