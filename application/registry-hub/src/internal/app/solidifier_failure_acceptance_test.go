package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestBlobFailuresClassifyRetainInputsAndReleaseWorker(t *testing.T) {
	for _, test := range []struct {
		name      string
		storage   error
		method    string
		class     string
		permanent bool
	}{
		{name: "filesystem unavailable", storage: storage.ErrUnavailable, class: "storage"},
		{name: "filesystem timeout", storage: storage.ErrTimeout, class: "storage"},
		{name: "filesystem permission", storage: storage.ErrPermissionDenied, class: "storage"},
		{name: "Distribution server error", method: http.MethodHead, class: "distribution"},
		{name: "upload creation failure", method: http.MethodPost, class: "distribution"},
		{name: "invalid local digest", method: "invalid", class: "distribution_invalid", permanent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("retain this input across worker failure"), "application/octet-stream")
			descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
			root := f.root("failure-classes", descriptor)
			var yielded *tasks.Yield
			if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
				t.Fatalf("prepare root = %v", err)
			}
			intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: blob.Digest})
			if err != nil {
				t.Fatal(err)
			}
			cache := f.service.cache
			if test.storage != nil {
				f.processor.cache = artifact.NewCache(blobCacheReadFailureStore{Store: f.service.store, failure: test.storage})
			}
			if test.method != "" {
				registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if test.method == "invalid" {
						w.Header().Set("Content-Length", "1")
						w.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256([]byte("wrong bytes")))
						w.WriteHeader(http.StatusOK)
						return
					}
					if r.Method == test.method {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					f.serveHTTP(w, r)
				}))
				f.processor.registry, f.service.registry = registry, registry
			}
			var failure *tasks.Failure
			if err := f.step(intent.ID); !errors.As(err, &failure) || failure.Class != test.class || failure.Permanent != test.permanent {
				t.Fatalf("worker failure = %v, want class=%s permanent=%t", err, test.class, test.permanent)
			}
			current, err := f.service.tasks.Get(ctx, intent.ID)
			wantStatus := tasks.RetryWait
			if test.permanent {
				wantStatus = tasks.Failed
			}
			if err != nil || current.Status != wantStatus || len(f.service.solidificationSlots) != 0 {
				t.Fatalf("failed task=%#v error=%v workers=%d", current, err, len(f.service.solidificationSlots))
			}
			if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC after worker failure = %v", err)
			}
			if removed, err := cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
				t.Fatalf("failed input cleanup = %d, %v", removed, err)
			}
			cached, err := cache.GetAnyRetained(ctx, root.Authority, root.Repository, blob.Digest)
			if err != nil {
				t.Fatalf("failed input was lost: %v", err)
			}
			_ = cached.Body.Close()
		})
	}
}
