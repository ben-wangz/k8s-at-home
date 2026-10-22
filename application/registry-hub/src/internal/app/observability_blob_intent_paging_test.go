package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestBlobIntentMetricsAggregatePagesAndRetainSnapshotOnFailure(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1<<20)
	store := &metricsPagedStore{Store: f.service.store}
	f.service.records = tasks.NewRecords(store)
	for i, state := range []tasks.BlobIntentState{tasks.IntentReady, tasks.IntentVerified, tasks.IntentFailed, tasks.IntentVerified, tasks.IntentReady} {
		identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "private/repo",
			Digest: artifact.CalculateSHA256([]byte(fmt.Sprint(i)))}
		intent, _, err := f.service.records.EnsureBlobIntent(ctx, identity, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.records.SetBlobIntentState(ctx, intent.ID, state, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	store.pages = 0
	f.service.refreshBlobIntentMetrics(ctx)
	if store.pages != 3 {
		t.Fatalf("metric snapshot pages = %d, want 3", store.pages)
	}
	assertSnapshot := func() {
		t.Helper()
		response := httptest.NewRecorder()
		f.service.metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		for _, sample := range []string{
			`registry_hub_blob_intents{state="ready"} 2`,
			`registry_hub_blob_intents{state="verified_local"} 2`,
			`registry_hub_blob_intents{state="failed"} 1`,
		} {
			if !strings.Contains(response.Body.String(), sample) {
				t.Fatalf("metric snapshot omits %s", sample)
			}
		}
		if strings.Contains(response.Body.String(), "private/repo") {
			t.Fatal("metric snapshot exposes a repository label")
		}
	}
	assertSnapshot()
	store.failLaterPage = true
	f.service.refreshBlobIntentMetrics(ctx)
	assertSnapshot()
}

type metricsPagedStore struct {
	storage.Store
	pages         int
	failLaterPage bool
}

func (s *metricsPagedStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if prefix == "solidification/blobs/intents/" {
		s.pages++
		if s.failLaterPage && token != "" {
			return nil, "", storage.ErrUnavailable
		}
		limit = min(limit, 2)
	}
	return s.Store.List(ctx, prefix, token, limit)
}
