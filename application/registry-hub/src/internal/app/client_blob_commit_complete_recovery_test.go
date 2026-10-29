package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestCommittedCacheSurvivesDownloadCleanupFailureWithoutAnotherFetch(t *testing.T) {
	for _, stage := range []string{"chunks", "state", "quota"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			body := "download cleanup interrupted after verified cache commit"
			digest := artifact.CalculateSHA256([]byte(body))
			remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, body: body, contentType: "application/octet-stream"}}}
			service, _, _ := newDiscoveryTestService(t, remote)
			faults := &downloadCleanupFailureStore{Store: service.store, stage: stage}
			service.store = faults
			service.cfg.Limits.ObjectBytes = 1 << 20
			path := "/v2/ghcr.io/team/image/blobs/" + digest
			routed, err := route.Parse(path)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			service.serveUpstreamBlob(response, httptest.NewRequest(http.MethodGet, path, nil), routed, &requestMeta{})
			if response.Code != http.StatusOK || response.Body.String() != body || faults.triggered || len(remote.calls) != 1 {
				t.Fatalf("committed response status=%d fault=%t fetches=%d", response.Code, faults.triggered, len(remote.calls))
			}
			for n := 0; n < 16 && !faults.triggered; n++ {
				_, _ = backgroundTestPass(service, "download_cleanup")
			}
			if !faults.triggered {
				t.Fatal("cleanup fault was not exercised")
			}
			if reserved, sessions, err := service.downloadManager().Usage(ctx); err != nil || reserved == 0 || sessions != 1 {
				t.Fatalf("pending cleanup quota=%d sessions=%d err=%v", reserved, sessions, err)
			}
			service.cache = artifact.NewCache(service.store)
			service.records = tasks.NewRecords(service.store)
			response = httptest.NewRecorder()
			service.serveUpstreamBlob(response, httptest.NewRequest(http.MethodGet, path, nil), routed, &requestMeta{})
			if response.Code != http.StatusOK || response.Body.String() != body || len(remote.calls) != 1 {
				t.Fatalf("recovered cache status=%d body=%q fetches=%d", response.Code, response.Body.String(), len(remote.calls))
			}
			identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}
			if err := service.processCachedBlobIntent(ctx, identity); err != nil {
				t.Fatal(err)
			}
			intent, err := service.records.GetBlobIntentByIdentity(ctx, identity)
			if err != nil || intent.State != tasks.IntentReady || intent.CacheVersion == "" {
				t.Fatalf("recovered activation = %#v, error=%v", intent, err)
			}
			drainBackgroundWork(t, service, "download_cleanup")
			if reserved, sessions, err := service.downloadManager().Usage(ctx); err != nil || reserved != 0 || sessions != 0 {
				t.Fatalf("recovered download quota = %d bytes, %d sessions, %v", reserved, sessions, err)
			}
		})
	}
}

type downloadCleanupFailureStore struct {
	storage.Store
	stage     string
	triggered bool
}

func (s *downloadCleanupFailureStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if !s.triggered && strings.HasPrefix(key, "downloads/v1/") &&
		(s.stage == "chunks" && strings.Contains(key, "/chunks/") || s.stage == "state" && strings.HasSuffix(key, "/state.json")) {
		s.triggered = true
		return storage.ErrUnavailable
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func (s *downloadCleanupFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if !s.triggered && s.stage == "quota" && key == "downloads/v1/quota.json" {
		data, err := io.ReadAll(body)
		if err != nil {
			return storage.Entry{}, err
		}
		body = strings.NewReader(string(data))
		if strings.Contains(string(data), `"reservations":{}`) {
			s.triggered = true
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}
