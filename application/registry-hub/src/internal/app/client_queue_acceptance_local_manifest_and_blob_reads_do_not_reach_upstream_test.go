package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
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
	if got := taskCount(t, manager); got != 0 {
		t.Fatalf("blob fallback created %d solidification tasks", got)
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

func TestUpstreamBlobValidationNeverCompletesPartialOrInvalidObjects(t *testing.T) {
	expected := []byte("complete blob")
	digest := artifact.CalculateSHA256(expected)
	for _, test := range []struct {
		name   string
		result upstreamResult
	}{
		{
			name: "unsolicited partial response",
			result: upstreamResult{status: http.StatusPartialContent, body: "complete", headers: http.Header{
				"Content-Range": []string{"bytes 0-7/13"},
			}},
		},
		{name: "wrong request digest", result: upstreamResult{status: http.StatusOK, body: "different blob"}},
		{name: "wrong declared length", result: upstreamResult{status: http.StatusOK, body: string(expected), contentLength: int64(len(expected) + 1), contentLengthSet: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &scriptedUpstream{results: []upstreamResult{test.result}}
			service, _, _ := newDiscoveryTestService(t, remote)
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
			recorder := httptest.NewRecorder()
			service.serveRead(recorder, httptest.NewRequest(http.MethodGet, path, nil), routed, make(url.Values), &requestMeta{})
			if recorder.Code < http.StatusBadRequest {
				t.Fatalf("invalid upstream object returned success status %d", recorder.Code)
			}
			if len(remote.calls) != 1 || remote.calls[0].rangeHeader != "" {
				t.Fatalf("upstream calls = %#v, want one full-object request", remote.calls)
			}
			if _, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, digest); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid response left a cache object: %v", err)
			}
		})
	}
}

func TestClientRangeIsServedFromVerifiedCompleteBlob(t *testing.T) {
	blob := []byte("complete ranged blob")
	digest := artifact.CalculateSHA256(blob)
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: string(blob)}}}
	service, _, _ := newDiscoveryTestService(t, remote)
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
	request.Header.Set("Range", "bytes=2-6")
	recorder := httptest.NewRecorder()
	service.serveRead(recorder, request, routed, make(url.Values), &requestMeta{})
	if recorder.Code != http.StatusPartialContent || recorder.Body.String() != string(blob[2:7]) || recorder.Header().Get("Content-Range") != "bytes 2-6/"+strconv.Itoa(len(blob)) {
		t.Fatalf("range response = %d body=%q range=%q", recorder.Code, recorder.Body.String(), recorder.Header().Get("Content-Range"))
	}
	if len(remote.calls) != 1 || remote.calls[0].rangeHeader != "" {
		t.Fatalf("upstream calls = %#v, want one full-object request without Range", remote.calls)
	}
	cached, err := service.cache.GetAny(context.Background(), routed.Upstream, routed.RemoteRepo, digest)
	if err != nil {
		t.Fatalf("verified complete object was not cached: %v", err)
	}
	cachedData, readErr := io.ReadAll(cached.Body)
	_ = cached.Body.Close()
	if readErr != nil || string(cachedData) != string(blob) {
		t.Fatalf("cached object = %q, read error = %v", cachedData, readErr)
	}
}
