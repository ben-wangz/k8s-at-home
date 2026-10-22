package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/route"
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
	service.records = tasks.NewRecords(store)
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
	recorder, _ := serveDiscoveryRequestWithMeta(t, service, routed, accept)
	return recorder
}

func serveDiscoveryRequestWithMeta(t *testing.T, service *Service, routed route.Route, accept string) (*httptest.ResponseRecorder, *requestMeta) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v2/ghcr.io/team/image/manifests/latest", nil)
	request.Header.Set("Accept", accept)
	recorder := httptest.NewRecorder()
	meta := &requestMeta{}
	service.serveUpstreamManifest(recorder, request, routed, meta)
	return recorder, meta
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

func TestManifestDiscoveryCombinesAcceptHeaderFields(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{
		status: http.StatusOK, contentType: ociIndexMediaType, body: ociIndexBody,
	}}}
	service, _, routed := newDiscoveryTestService(t, remote)
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Add("Accept", ociManifestMediaType)
	request.Header.Add("Accept", ociIndexMediaType)
	recorder := httptest.NewRecorder()
	service.serveUpstreamManifest(recorder, request, routed, &requestMeta{})
	if recorder.Code != http.StatusOK || recorder.Body.String() != ociIndexBody {
		t.Fatalf("response status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 1 || !strings.Contains(remote.calls[0].accept, ociIndexMediaType) {
		t.Fatalf("upstream calls = %#v, want one request with both Accept lines combined", remote.calls)
	}
}

func TestMutableTagRevalidatesWhilePreviousRootIsPending(t *testing.T) {
	updated := strings.Replace(ociManifestBody, `"layers":[]`, `"layers":[],"annotations":{"revision":"new"}`, 1)
	remote := &scriptedUpstream{results: []upstreamResult{
		{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody},
		{status: http.StatusOK, contentType: ociManifestMediaType, body: updated},
	}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	first := serveDiscoveryRequest(t, service, routed, ociManifestMediaType)
	if first.Code != http.StatusOK || first.Body.String() != ociManifestBody {
		t.Fatalf("first tag response = %d %q", first.Code, first.Body.String())
	}
	previous := discoveryTask(t, manager)
	second := serveDiscoveryRequest(t, service, routed, ociManifestMediaType)
	if second.Code != http.StatusOK || second.Body.String() != updated {
		t.Fatalf("second tag response = %d %q, want updated upstream representation", second.Code, second.Body.String())
	}
	if got, want := second.Header().Get("Docker-Content-Digest"), artifact.CalculateSHA256([]byte(updated)); got != want {
		t.Fatalf("second tag digest = %q, want %q", got, want)
	}
	if len(remote.calls) != 2 {
		t.Fatalf("upstream manifest calls = %d, want one resolution per mutable tag GET", len(remote.calls))
	}
	current, _, err := manager.List(context.Background(), "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	foundUpdated := false
	for _, task := range current {
		if task.Reference == routed.Reference && task.RootDigest == artifact.CalculateSHA256([]byte(updated)) && task.ID != previous.ID {
			foundUpdated = true
		}
	}
	if !foundUpdated {
		t.Fatal("changed tag resolution did not create or join the updated root snapshot")
	}
}
