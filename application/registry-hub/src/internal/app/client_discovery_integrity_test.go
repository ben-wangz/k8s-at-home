package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestInvalidUpstreamManifestNeverCreatesCompleteCacheOrTask(t *testing.T) {
	validDigest := artifact.CalculateSHA256([]byte(ociManifestBody))
	inlineManifest, err := json.Marshal(artifact.Manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestMediaType,
		Config: &artifact.Descriptor{
			MediaType: "application/vnd.oci.empty.v1+json",
			Digest:    artifact.CalculateSHA256([]byte("different data")),
			Size:      2,
			Data:      json.RawMessage(`"e30="`),
		},
		Layers: []artifact.Descriptor{},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		result     upstreamResult
		errorClass string
	}{
		{
			name: "wrong Content-Length", errorClass: "manifest_invalid",
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				contentLength: int64(len(ociManifestBody) + 1), contentLengthSet: true},
		},
		{
			name: "descriptor.data digest mismatch", errorClass: "manifest_invalid",
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: string(inlineManifest)},
		},
		{
			name: "wrong Docker-Content-Digest", errorClass: "digest_mismatch",
			result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: ociManifestBody,
				headers: http.Header{"Docker-Content-Digest": []string{"sha256:" + strings.Repeat("f", 64)}}},
		},
		{name: "invalid Content-Type", errorClass: "manifest_invalid", result: upstreamResult{status: http.StatusOK, contentType: "text/plain", body: ociManifestBody}},
		{name: "invalid manifest structure", errorClass: "manifest_invalid", result: upstreamResult{status: http.StatusOK, contentType: ociManifestMediaType, body: `{"schemaVersion":1}`}},
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
			if task.Status != tasks.Failed || task.ErrorClass != test.errorClass {
				t.Fatalf("invalid manifest task state = %#v", task)
			}
			if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, validDigest); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid manifest became a complete cache object: %v", err)
			}
		})
	}
}

func TestUpstreamAuthorizationFailureKeepsTaskClassGeneric(t *testing.T) {
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusUnauthorized}}}
	service, manager, routed := newDiscoveryTestService(t, remote)
	request := httptest.NewRequest(http.MethodGet, routed.RawPath, nil)
	request.Header.Set("Accept", ociManifestMediaType)
	recorder := httptest.NewRecorder()
	meta := &requestMeta{}

	service.serveUpstreamManifest(recorder, request, routed, meta)
	if recorder.Code != http.StatusUnauthorized || meta.errorClass != "upstream_auth" {
		t.Fatalf("authorization response status=%d errorClass=%q, want 401/upstream_auth", recorder.Code, meta.errorClass)
	}
	task := discoveryTask(t, manager)
	if task.Status != tasks.Failed || task.Phase != "discovery" || task.ErrorClass != "upstream" || task.RootDigest != "" || task.RootPublished {
		t.Fatalf("authorization failure task state = %#v", task)
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
			wantClass := "manifest_invalid"
			if test.name != "body mediaType differs from response Content-Type" {
				wantClass = "digest_mismatch"
			}
			if task.Status != tasks.Failed || task.ErrorClass != wantClass || task.RootDigest != "" || task.RootPublished {
				t.Fatalf("invalid digest-addressed response advanced task state: %#v", task)
			}
			if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, test.requestDigest); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid digest-addressed response was cached: %v", err)
			}
		})
	}
}
