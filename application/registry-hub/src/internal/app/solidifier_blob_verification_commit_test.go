package app

import (
	"context"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync/atomic"
	"testing"
)

type observationFaultStore struct {
	storage.Store
	mode, prefix string
	fired        atomic.Bool
}

func (s *observationFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if !strings.HasPrefix(key, s.prefix) || !s.fired.CompareAndSwap(false, true) {
		return s.Store.Put(ctx, key, body, size, options)
	}
	if s.mode == "failed" {
		return storage.Entry{}, storage.ErrUnavailable
	}
	info, err := s.Store.Put(ctx, key, body, size, options)
	if err != nil {
		return info, err
	}
	return info, storage.ErrUnknownCommit
}

func TestBlobVerificationObservationCommitRecovery(t *testing.T) {
	for _, tc := range []struct{ name, mode, prefix string }{
		{"observation_failed", "failed", "solidification/observations/"},
		{"observation_unknown_committed", "unknown_committed", "solidification/observations/"},
		{"index_failed", "failed", "solidification/observation-index/"},
		{"index_unknown_committed", "unknown_committed", "solidification/observation-index/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			fault := &observationFaultStore{Store: store, mode: tc.mode, prefix: tc.prefix}
			f := newStagedAcceptanceFixtureWithStore(t, 1<<20, fault)
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
			root := f.root("observation-commit", manifest)
			body := []byte("verification survives observation commit uncertainty")
			digest := artifact.CalculateSHA256(body)
			f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
			blob := contentRef{digest: digest, size: int64(len(body))}
			path := "/v2/" + f.processor.internalName(root.Authority, root.Repository) + "/blobs/" + digest
			var reads atomic.Int32
			f.beforeRequest = func(r *http.Request) {
				if r.Method == http.MethodGet {
					reads.Add(1)
				}
			}
			ctx, release, err := acquireContentLease(context.Background(), f.service.tasks.Leases(), root.Authority, root.Repository, digest, "observation-commit")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			err = f.processor.verifyOrReuseLocalBlob(ctx, &root, blob, "", path)
			if !fault.fired.Load() || (err == nil) != (tc.mode == "unknown_committed") {
				t.Fatalf("first verification: fault=%t err=%v", fault.fired.Load(), err)
			}
			if err := f.processor.verifyOrReuseLocalBlob(ctx, &root, blob, "", path); err != nil {
				t.Fatalf("verification retry: %v", err)
			}
			wantReads := int32(1)
			if tc.mode == "failed" {
				wantReads = 2
			}
			if reads.Load() != wantReads {
				t.Fatalf("body reads = %d, want %d", reads.Load(), wantReads)
			}
			if _, err := f.service.records.FindLocalObservation(ctx, root.Authority, root.Repository, tasks.GraphBlob, digest, blob.size, 0, f.service.gate.State().Epoch); err != nil {
				t.Fatalf("verified observation missing after retry: %v", err)
			}
		})
	}
}
