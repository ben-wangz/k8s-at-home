package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strconv"
	"testing"
)

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
