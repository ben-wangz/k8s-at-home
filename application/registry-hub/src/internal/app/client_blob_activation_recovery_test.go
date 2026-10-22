package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestDirectCachedBlobActivatesAfterRestartIncludingEmptyPayload(t *testing.T) {
	for _, body := range []string{"", "direct blob without a manifest"} {
		for _, failActivation := range []bool{false, true} {
			name := "nonempty"
			if body == "" {
				name = "empty"
			}
			if failActivation {
				name += "/activation_failure"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				f := newStagedAcceptanceFixture(t, 1<<20)
				blob := f.cacheObject([]byte(body), "application/octet-stream")
				faults := &blobActivationFailureStore{Store: f.service.store, enabled: failActivation}
				f.service.records = tasks.NewRecords(faults)
				path := "/v2/ghcr.io/team/image/blobs/" + blob.Digest
				routed, err := route.Parse(path)
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				f.service.serveUpstreamBlob(response, httptest.NewRequest(http.MethodGet, path, nil), routed, &requestMeta{})
				if response.Code != http.StatusOK || response.Body.String() != body {
					t.Fatalf("direct blob response = %d %q", response.Code, response.Body.String())
				}
				identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest}
				intent, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
				if err != nil {
					t.Fatal(err)
				}
				if failActivation && (!faults.triggered || intent.State != tasks.IntentWaitingCache) {
					t.Fatalf("failed activation did not retain durable pending intent: %#v", intent)
				}
				f.service.records = tasks.NewRecords(f.service.store)
				if err := f.service.processCachedBlobIntent(ctx, identity); err != nil {
					t.Fatalf("recover activation from committed cache: %v", err)
				}
				intent, err = f.service.records.GetBlobIntentByIdentity(ctx, identity)
				if err != nil || intent.State != tasks.IntentReady || intent.ActualSize != blob.Size || intent.CacheVersion == "" {
					t.Fatalf("recovered intent = %#v, error=%v", intent, err)
				}
				f.transfer(blob)
				f.mu.Lock()
				uploads := f.uploads[blob.Digest]
				f.mu.Unlock()
				if uploads != 1 || f.service.gate.State().ProtectionSessionCount != 1 {
					t.Fatalf("rootless transfer uploads=%d gate=%#v", uploads, f.service.gate.State())
				}
			})
		}
	}
}

type blobActivationFailureStore struct {
	storage.Store
	enabled, triggered bool
}

func (s *blobActivationFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.enabled && !s.triggered && strings.HasPrefix(key, "solidification/blobs/intents/") {
		data, err := io.ReadAll(body)
		if err != nil {
			return storage.Entry{}, err
		}
		body = bytes.NewReader(data)
		var intent tasks.BlobIntent
		if err := json.Unmarshal(data, &intent); err != nil {
			return storage.Entry{}, err
		}
		if intent.State == tasks.IntentReady {
			s.triggered = true
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}
