package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
)

func TestPlatformBlobRepullUsesDistributionThenCacheThenUpstream(t *testing.T) {
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		for _, source := range []string{"local", "cache", "upstream"} {
			t.Run(platform+"/"+source, func(t *testing.T) {
				body := []byte("platform layer for " + platform)
				digest := artifact.CalculateSHA256(body)
				remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: string(body), contentType: "application/octet-stream"}}}
				service, _, _ := newDiscoveryTestService(t, remote)
				service.cfg.Limits.ObjectBytes = 1 << 20
				if source != "upstream" {
					if err := service.cache.Put(context.Background(), "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
						t.Fatal(err)
					}
				}
				service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if source == "local" {
						w.Header().Set("Content-Type", "application/octet-stream")
						w.Header().Set("Content-Length", strconv.Itoa(len(body)))
						w.Header().Set("Docker-Content-Digest", digest)
						_, _ = w.Write(body)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
				}))
				path := "/v2/ghcr.io/team/image/blobs/" + digest
				routed, err := route.Parse(path)
				if err != nil {
					t.Fatal(err)
				}
				meta := &requestMeta{}
				response := httptest.NewRecorder()
				service.serveRead(response, httptest.NewRequest(http.MethodGet, path, nil), routed, make(url.Values), meta)
				wantFetches := 0
				if source == "upstream" {
					wantFetches = 1
				}
				if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), body) || meta.cacheSource != source || len(remote.calls) != wantFetches {
					t.Fatalf("source=%s status=%d body=%q fetches=%d", meta.cacheSource, response.Code, response.Body.Bytes(), len(remote.calls))
				}
			})
		}
	}
}

func TestCacheStorageErrorsNeverBecomeUpstreamMisses(t *testing.T) {
	for _, failure := range []error{storage.ErrUnavailable, storage.ErrTimeout, storage.ErrPermissionDenied} {
		t.Run(failure.Error(), func(t *testing.T) {
			remote := &scriptedUpstream{}
			service, _, _ := newDiscoveryTestService(t, remote)
			service.cache = artifact.NewCache(blobCacheReadFailureStore{Store: service.store, failure: failure})
			routed, err := route.Parse("/v2/ghcr.io/team/image/blobs/" + artifact.CalculateSHA256([]byte("unavailable cache")))
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			service.serveUpstreamBlob(response, httptest.NewRequest(http.MethodGet, routed.RawPath, nil), routed, &requestMeta{})
			if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") == "" || len(remote.calls) != 0 {
				t.Fatalf("storage failure status=%d Retry-After=%q fetches=%d", response.Code, response.Header().Get("Retry-After"), len(remote.calls))
			}
		})
	}
}

type blobCacheReadFailureStore struct {
	storage.Store
	failure error
}

func (s blobCacheReadFailureStore) Get(context.Context, string) (storage.Object, error) {
	return storage.Object{}, s.failure
}
