package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestBlobResultPersistenceFailureRetainsCacheQuotaAndGCProtection(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		commit bool
	}{
		{name: "unavailable", err: storage.ErrUnavailable},
		{name: "timeout", err: storage.ErrTimeout},
		{name: "unknown after commit", err: storage.ErrUnknownCommit, commit: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("verified upload with an uncertain result record"), "application/octet-stream")
			descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
				Config: &blob, Layers: []artifact.Descriptor{}})
			root := f.root("result-failure", descriptor)
			var yielded *tasks.Yield
			if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
				t.Fatalf("prepare root = %v", err)
			}
			intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{
				Authority: root.Authority, Repository: root.Repository, Digest: blob.Digest,
			})
			if err != nil {
				t.Fatal(err)
			}
			faults := &blobResultFailureStore{Store: f.service.store, failure: test.err, commit: test.commit}
			f.service.records = tasks.NewRecords(faults)
			wantError := test.err
			if test.commit {
				wantError = storage.ErrUnavailable
			}
			if err := f.step(intent.ID); !errors.Is(err, wantError) {
				t.Fatalf("result persistence = %v, want %v", err, wantError)
			}
			if !faults.triggered {
				t.Fatal("result persistence fault did not run")
			}
			state := f.service.gate.State()
			if state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes == 0 || state.ProtectedCacheBytes != blob.Size {
				t.Fatalf("failed result persistence lost quota or protection: %#v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC after uncertain result = %v", err)
			}
			if len(f.service.solidificationSlots) != 0 {
				t.Fatal("state persistence failure retained a worker slot")
			}
			if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
				t.Fatalf("cleanup after result failure = %d, %v", removed, err)
			}
			f.service.records = tasks.NewRecords(f.service.store)
			f.transfer(blob)
			if err := f.step(root.ID); err != nil {
				t.Fatalf("root finalization after result recovery: %v", err)
			}
			if state := f.service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 || state.ProtectedCacheBytes != 0 {
				t.Fatalf("recovered anchored scope did not release quota: %#v", state)
			}
			f.mu.Lock()
			uploads := f.uploads[blob.Digest]
			f.mu.Unlock()
			if uploads != 1 {
				t.Fatalf("payload uploads across result recovery = %d, want 1", uploads)
			}
		})
	}
}

type blobResultFailureStore struct {
	storage.Store
	failure   error
	commit    bool
	triggered bool
}

func (s *blobResultFailureStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.triggered && s.commit && strings.HasPrefix(key, "solidification/blobs/intents/") {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *blobResultFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if !s.triggered && strings.HasPrefix(key, "solidification/blobs/intents/") {
		data, err := io.ReadAll(body)
		if err != nil {
			return storage.Entry{}, err
		}
		body = bytes.NewReader(data)
		var intent tasks.BlobIntent
		if err := json.Unmarshal(data, &intent); err != nil {
			return storage.Entry{}, err
		}
		if intent.State == tasks.IntentVerified {
			s.triggered = true
			if s.commit {
				if _, err := s.Store.Put(ctx, key, body, size, options); err != nil {
					return storage.Entry{}, err
				}
			}
			return storage.Entry{}, s.failure
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}
