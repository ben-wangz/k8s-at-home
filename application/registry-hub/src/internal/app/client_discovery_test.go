package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

const (
	ociIndexMediaType       = "application/vnd.oci.image.index.v1+json"
	dockerListMediaType     = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifestMediaType    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestMediaType = "application/vnd.docker.distribution.manifest.v2+json"
	ociIndexBody            = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`
	dockerListBody          = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json","manifests":[]}`
	ociManifestBody         = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881","size":1},"layers":[]}`
	dockerManifestBody      = `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":"sha256:2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881","size":1},"layers":[]}`
)

func newDiscoveryTestService(t *testing.T, remote *scriptedUpstream) (*Service, *tasks.Manager, route.Route) {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	service := retryTestService(remote)
	service.cfg = config.Config{Limits: config.Limits{
		ManifestBytes: 1 << 20, MaxDescriptors: 100, DescriptorDataBytes: 1 << 20,
	}}
	service.store = store
	service.cache = artifact.NewCache(store)
	service.tasks = manager
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		manager.Close()
		_ = store.Close()
	})
	return service, manager, routed
}

func serveDiscoveryRequest(t *testing.T, service *Service, routed route.Route, accept string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/team/image/manifests/latest", nil)
	request.Header.Set("Accept", accept)
	recorder := httptest.NewRecorder()
	service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
	return recorder
}

func discoveryTask(t *testing.T, manager *tasks.Manager) tasks.Task {
	t.Helper()
	items, _, err := manager.List(context.Background(), "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("task count = %d, want 1", len(items))
	}
	return items[0]
}

func cachedBody(t *testing.T, service *Service, authority, repository, digest, mediaType string) []byte {
	t.Helper()
	object, err := service.cache.Get(context.Background(), authority, repository, digest, mediaType)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestManifestDiscoveryAcceptsSupportedDockerAndOCIRepresentations(t *testing.T) {
	for _, test := range []struct {
		name      string
		mediaType string
		body      string
	}{
		{name: "OCI index", mediaType: ociIndexMediaType, body: ociIndexBody},
		{name: "Docker manifest list", mediaType: dockerListMediaType, body: dockerListBody},
		{name: "OCI manifest", mediaType: ociManifestMediaType, body: ociManifestBody},
		{name: "Docker Schema 2 manifest", mediaType: dockerManifestMediaType, body: dockerManifestBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{{
				status: http.StatusOK, contentType: test.mediaType, body: test.body,
			}}}
			service, _, routed := newDiscoveryTestService(t, remote)
			recorder := serveDiscoveryRequest(t, service, routed, "*/*")
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if recorder.Body.String() != test.body {
				t.Fatal("discovery response bytes were rewritten")
			}
			if recorder.Header().Get("Content-Type") != test.mediaType {
				t.Fatalf("Content-Type = %q, want %q", recorder.Header().Get("Content-Type"), test.mediaType)
			}
			if len(remote.calls) != 1 || remote.calls[0].method != http.MethodGet {
				t.Fatalf("upstream calls = %#v, want one GET", remote.calls)
			}
			for _, supported := range []string{ociIndexMediaType, dockerListMediaType, ociManifestMediaType, dockerManifestMediaType} {
				if !strings.Contains(remote.calls[0].accept, supported) {
					t.Errorf("discovery Accept %q omits %q", remote.calls[0].accept, supported)
				}
			}
		})
	}
}

func TestInvalidUpstreamManifestNeverCreatesCompleteCacheOrTask(t *testing.T) {
	validDigest := artifact.CalculateSHA256([]byte(ociManifestBody))
	for _, test := range []struct {
		name   string
		result upstreamResult
	}{
		{
			name: "wrong Content-Length",
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				contentLength: int64(len(ociManifestBody) + 1), contentLengthSet: true},
		},
		{
			name: "wrong Docker-Content-Digest",
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				headers: http.Header{"Docker-Content-Digest": []string{"sha256:" + strings.Repeat("f", 64)}}},
		},
		{name: "invalid Content-Type", result: upstreamResult{status: http.StatusOK, contentType: "text/plain", body: ociManifestBody}},
		{name: "invalid manifest structure", result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: `{"schemaVersion":1}`}},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{test.result}}
			service, manager, routed := newDiscoveryTestService(t, remote)
			request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
			request.Header.Set("Accept", ociManifestMediaType)
			recorder := httptest.NewRecorder()
			service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("invalid manifest returned status %d", recorder.Code)
			}
			task := discoveryTask(t, manager)
			if task.Status != tasks.Failed || task.ErrorClass != "manifest_invalid" {
				t.Fatalf("invalid manifest task state = %#v", task)
			}
			if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, validDigest); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid manifest became a complete cache object: %v", err)
			}
		})
	}
}

func TestUnsupportedAcceptReturns406WithoutTaskCacheOrUpstreamRequest(t *testing.T) {
	remote := &scriptedUpstream{}
	service, manager, routed := newDiscoveryTestService(t, remote)
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
		request.Header.Set("Accept", "application/json")
		recorder := httptest.NewRecorder()
		service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
		if recorder.Code != http.StatusNotAcceptable {
			t.Fatalf("attempt %d status = %d, want 406", attempt+1, recorder.Code)
		}
	}
	if len(remote.calls) != 0 {
		t.Fatalf("unsupported Accept triggered upstream requests: %#v", remote.calls)
	}
	if tasks, _, err := manager.List(context.Background(), "", "", 10); err != nil || len(tasks) != 0 {
		t.Fatalf("unsupported Accept created tasks: %#v, error=%v", tasks, err)
	}
	objects, _, err := service.store.List(context.Background(), "cache/objects/", "", 100)
	if err != nil || len(objects) != 0 {
		t.Fatalf("unsupported Accept created cache objects: %#v, error=%v", objects, err)
	}
}

func TestDigestManifestRejectsUpstreamDigestOrMediaTypeReplacement(t *testing.T) {
	bodyDigest := artifact.CalculateSHA256([]byte(ociManifestBody))
	requestDigest := artifact.CalculateSHA256([]byte("different requested manifest"))
	for _, test := range []struct {
		name          string
		requestDigest string
		result        upstreamResult
	}{
		{
			name: "Docker-Content-Digest differs from request", requestDigest: requestDigest,
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				headers: http.Header{"Docker-Content-Digest": []string{bodyDigest}}},
		},
		{
			name: "body digest differs from request header", requestDigest: requestDigest,
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				headers: http.Header{"Docker-Content-Digest": []string{requestDigest}}},
		},
		{
			name: "body mediaType differs from response Content-Type", requestDigest: artifact.CalculateSHA256([]byte(dockerManifestBody)),
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: dockerManifestBody},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{test.result}}
			service, manager, _ := newDiscoveryTestService(t, remote)
			path := "/v2/ghcr.io/team/image/manifests/" + test.requestDigest
			routed, err := route.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Accept", ociManifestMediaType)
			recorder := httptest.NewRecorder()
			service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
			}
			if len(remote.calls) != 1 {
				t.Fatalf("upstream calls = %d, want one", len(remote.calls))
			}
			task := discoveryTask(t, manager)
			if task.Status != tasks.Failed || task.ErrorClass != "manifest_invalid" || task.RootDigest != "" || task.RootPublished {
				t.Fatalf("invalid digest-addressed response advanced task state: %#v", task)
			}
			if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, test.requestDigest); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid digest-addressed response was cached: %v", err)
			}
		})
	}
}
