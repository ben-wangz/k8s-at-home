package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestUpstreamHeadWithoutDigestDoesNotRetryAsGetOrCompleteCache(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: "head response"}}}
	service, _, _ := newDiscoveryTestService(t, remote)
	service.cfg.Limits.ObjectBytes = 1 << 20
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
	}))
	digest := "sha256:" + strings.Repeat("a", 64)
	path := "/v2/ghcr.io/team/image/blobs/" + digest
	routed, err := route.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, httptest.NewRequest(http.MethodHead, path, nil), routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("HEAD without digest returned status %d, want 502", recorder.Code)
	}
	if len(remote.calls) != 1 || remote.calls[0].method != http.MethodHead {
		t.Fatalf("upstream calls = %#v, want one HEAD and no GET retry", remote.calls)
	}
	if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("HEAD created a complete cache object: %v", err)
	}
}

func TestReadOnlyDiscoveryAndBlobRequestsDoNotCreateSolidificationTasks(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		method string
		result []upstreamResult
		serve  func(*Service, http.ResponseWriter, *http.Request, route.Route)
	}{
		{
			name: "manifest HEAD", path: "/v2/ghcr.io/team/image/manifests/latest", method: http.MethodHead,
			result: []upstreamResult{{status: http.StatusNotFound}},
			serve: func(service *Service, writer http.ResponseWriter, request *http.Request, routed route.Route) {
				service.serveUpstreamHead(writer, request, routed, make(url.Values), &requestMeta{})
			},
		},
		{
			name: "tags list", path: "/v2/ghcr.io/team/image/tags/list", method: http.MethodGet,
			result: []upstreamResult{{status: http.StatusOK, body: `{"tags":[]}`}},
			serve: func(service *Service, writer http.ResponseWriter, request *http.Request, routed route.Route) {
				service.serveUpstreamTags(writer, request, routed, make(url.Values), &requestMeta{})
			},
		},
		{
			name: "Referrers discovery", path: "/v2/ghcr.io/team/image/referrers/sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", method: http.MethodGet,
			result: []upstreamResult{{status: http.StatusInternalServerError}},
			serve: func(service *Service, writer http.ResponseWriter, request *http.Request, routed route.Route) {
				service.serveUpstreamReferrers(writer, request, routed, make(url.Values), &requestMeta{})
			},
		},
		{
			name: "blob GET", path: "/v2/ghcr.io/team/image/blobs/sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881", method: http.MethodGet,
			result: []upstreamResult{{status: http.StatusNotFound}},
			serve: func(service *Service, writer http.ResponseWriter, request *http.Request, routed route.Route) {
				service.serveUpstreamBlob(writer, request, routed, &requestMeta{})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: test.result}
			service, manager, _ := newDiscoveryTestService(t, remote)
			service.cfg.Limits.MaxPageSize = 100
			service.cfg.Limits.MaxReferrerPages = 10
			routed, err := route.Parse(test.path)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Accept", ociManifestMediaType)
			recorder := httptest.NewRecorder()
			test.serve(service, recorder, request, routed)
			if got := taskCount(t, manager); got != 0 {
				t.Fatalf("created %d solidification tasks", got)
			}
		})
	}
}

func TestObjectLeaseKeyIncludesAuthorityRepositoryAndDigest(t *testing.T) {
	identity := objectLeaseKey("ghcr.io", "team/image", "sha256:abc")
	if len(identity) != 64 {
		t.Fatalf("object lease key length = %d, want 64 hexadecimal characters", len(identity))
	}
	for _, test := range []struct {
		authority  string
		repository string
		digest     string
	}{
		{authority: "docker.io", repository: "team/image", digest: "sha256:abc"},
		{authority: "ghcr.io", repository: "other/image", digest: "sha256:abc"},
		{authority: "ghcr.io", repository: "team/image", digest: "sha256:def"},
	} {
		if got := objectLeaseKey(test.authority, test.repository, test.digest); got == identity {
			t.Errorf("identity (%q, %q, %q) shares lease key %q", test.authority, test.repository, test.digest, got)
		}
	}
}

func TestSharedBlobWaiterTimeoutReturns503AndRetryAfter(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK}}}
	service, manager, _ := newDiscoveryTestService(t, remote)
	routed, err := route.Parse("/v2/ghcr.io/team/image/blobs/sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881")
	if err != nil {
		t.Fatal(err)
	}
	identity := "objects/" + objectLeaseKey(routed.Upstream, routed.RemoteRepo, routed.Reference)
	leader, err := manager.Leases().Acquire(context.Background(), identity, "leader", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(context.Background(), leader)

	err = service.cacheUpstreamBlobUntil(context.Background(), routed, time.Now().Add(-time.Second))
	var fetchErr *upstreamBlobFetchError
	if !errors.As(err, &fetchErr) || fetchErr.status != http.StatusServiceUnavailable || fetchErr.retryAfter != "5" {
		t.Fatalf("shared download wait error = %#v, want 503 with Retry-After 5", err)
	}
	recorder := httptest.NewRecorder()
	writeUpstreamBlobFetchError(recorder, routed.Endpoint, fetchErr)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") != "5" {
		t.Fatalf("waiter response = %d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if len(remote.calls) != 0 {
		t.Fatalf("waiter started another upstream request: %#v", remote.calls)
	}
}
