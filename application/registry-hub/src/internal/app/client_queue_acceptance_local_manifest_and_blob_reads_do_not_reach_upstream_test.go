package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"testing"
)

func TestLocalManifestAndBlobReadsDoNotReachUpstream(t *testing.T) {
	manifestDigest := artifact.CalculateSHA256([]byte(ociManifestBody))
	blob := []byte("local blob")
	blobDigest := artifact.CalculateSHA256(blob)
	for _, test := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "manifest GET", method: http.MethodGet, path: "/v2/ghcr.io/team/image/manifests/latest"},
		{name: "manifest HEAD", method: http.MethodHead, path: "/v2/ghcr.io/team/image/manifests/latest"},
		{name: "blob GET", method: http.MethodGet, path: "/v2/ghcr.io/team/image/blobs/" + blobDigest},
		{name: "blob HEAD", method: http.MethodHead, path: "/v2/ghcr.io/team/image/blobs/" + blobDigest},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{}
			service, manager, _ := newDiscoveryTestService(t, remote)
			service.cfg.Limits.ObjectBytes = 1 << 20
			service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if strings.Contains(request.URL.Path, "/manifests/") {
					writer.Header().Set("Content-Type", ociManifestMediaType)
					writer.Header().Set("Docker-Content-Digest", manifestDigest)
					writer.Header().Set("Content-Length", strconv.Itoa(len(ociManifestBody)))
					writer.WriteHeader(http.StatusOK)
					if request.Method == http.MethodGet {
						_, _ = io.WriteString(writer, ociManifestBody)
					}
					return
				}
				writer.Header().Set("Content-Type", "application/octet-stream")
				writer.Header().Set("Docker-Content-Digest", blobDigest)
				writer.Header().Set("Content-Length", strconv.Itoa(len(blob)))
				writer.WriteHeader(http.StatusOK)
				if request.Method == http.MethodGet {
					_, _ = writer.Write(blob)
				}
			}))
			routed, err := route.Parse(test.path)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, test.path, nil)
			request.Header.Set("Accept", ociManifestMediaType)
			recorder := httptest.NewRecorder()
			service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
			if recorder.Code != http.StatusOK || test.method == http.MethodHead && recorder.Body.Len() != 0 {
				t.Fatalf("local %s response = %d body=%q", test.name, recorder.Code, recorder.Body.String())
			}
			if len(remote.calls) != 0 {
				t.Fatalf("upstream calls = %#v, want none", remote.calls)
			}
			if got := taskCount(t, manager); got != 0 {
				t.Fatalf("created %d solidification tasks for local hit", got)
			}
		})
	}
}

func TestBlobFallsBackOnlyAfterConfirmedInternalNotFound(t *testing.T) {
	blob := []byte("verified upstream blob")
	digest := artifact.CalculateSHA256(blob)
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: string(blob)}}}
	service, manager, _ := newDiscoveryTestService(t, remote)
	service.cfg.Limits.ObjectBytes = 1 << 20
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
	}))
	path := "/v2/ghcr.io/team/image/blobs/" + digest
	routed, err := route.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusOK || recorder.Body.String() != string(blob) {
		t.Fatalf("fallback response = %d %q", recorder.Code, recorder.Body.String())
	}
	if len(remote.calls) != 1 || remote.calls[0].method != http.MethodGet {
		t.Fatalf("upstream calls = %#v, want one GET after confirmed local miss", remote.calls)
	}
	items, _, err := manager.List(context.Background(), "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Kind != tasks.BlobKind || items[0].BlobDigest != digest {
		t.Fatalf("cold blob read tasks = %#v, want one intent-backed blob task", items)
	}
}

func TestCachedBlobFailsWhenSolidificationIntentStoreIsUnavailable(t *testing.T) {
	blob := []byte("verified cached blob")
	digest := artifact.CalculateSHA256(blob)
	remote := &scriptedUpstream{}
	service, _, _ := newDiscoveryTestService(t, remote)
	if _, err := service.cache.PutBytes(context.Background(), "ghcr.io", "team/image", "application/octet-stream", blob); err != nil {
		t.Fatalf("put verified cache object: %v", err)
	}
	service.records = nil
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
	}))
	path := "/v2/ghcr.io/team/image/blobs/" + digest
	routed, err := route.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, httptest.NewRequest(http.MethodGet, path, nil), routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("cache hit response = %d Retry-After=%q, want retryable 503 before serving bytes", recorder.Code, recorder.Header().Get("Retry-After"))
	}
	if recorder.Body.String() == string(blob) {
		t.Fatal("served cache bytes without a durable solidification intent")
	}
	if len(remote.calls) != 0 {
		t.Fatalf("cache hit reached upstream: %#v", remote.calls)
	}
}

func TestUnconfirmedInternalBlobMissDoesNotReachUpstream(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		code   string
	}{
		{name: "server error", status: http.StatusInternalServerError, code: "BLOB_UNKNOWN"},
		{name: "unrelated not found", status: http.StatusNotFound, code: "UNSUPPORTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: "must not be fetched"}}}
			service, manager, _ := newDiscoveryTestService(t, remote)
			service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(test.status)
				_, _ = fmt.Fprintf(writer, `{"errors":[{"code":%q}]}`, test.code)
			}))
			path := "/v2/ghcr.io/team/image/blobs/sha256:" + strings.Repeat("a", 64)
			routed, err := route.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			service.serveRead(recorder, httptest.NewRequest(http.MethodGet, path, nil), routed, make(url.Values), &requestMeta{})
			if recorder.Code != test.status {
				t.Fatalf("internal status %d became %d", test.status, recorder.Code)
			}
			if len(remote.calls) != 0 {
				t.Fatalf("upstream calls = %#v, want none for unconfirmed internal miss", remote.calls)
			}
			if got := taskCount(t, manager); got != 0 {
				t.Fatalf("created %d solidification tasks", got)
			}
		})
	}
}
