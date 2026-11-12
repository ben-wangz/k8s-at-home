package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"sync/atomic"
	"testing"
	"time"
)

type payloadCountingStore struct {
	storage.Store
	opener      storage.PayloadOpener
	fingerprint storage.Fingerprinter
	readBytes   atomic.Int64
	beforeRead  func()
}

func TestLegacyCacheGetUpgradesOnceThenStreamsWithoutRehash(t *testing.T) {
	cache, store, digest, data, _ := payloadCacheFixture(t)
	identity := cacheIdentity("example.test", "team/image", digest)
	mediaType := "application/octet-stream"
	metadataKey := cacheMetadataKey(identity, mediaType)
	metadataObject, err := store.Store.Get(context.Background(), metadataKey)
	if err != nil {
		t.Fatal(err)
	}
	var metadata cacheMetadata
	if err := json.NewDecoder(metadataObject.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	_ = metadataObject.Body.Close()
	metadata.IntegrityState, metadata.IntegrityDigest, metadata.IntegrityMethod = "", "", ""
	metadata.IntegritySize, metadata.IntegrityVerifiedAt = 0, time.Time{}
	metadataJSON, _ := json.Marshal(metadata)
	if _, err := store.Store.Put(context.Background(), metadataKey, bytes.NewReader(metadataJSON), int64(len(metadataJSON)), storage.PutOptions{IfMatch: metadataObject.Version}); err != nil {
		t.Fatal(err)
	}
	markerKey := cacheCompleteKey(identity, mediaType)
	markerObject, err := store.Store.Get(context.Background(), markerKey)
	if err != nil {
		t.Fatal(err)
	}
	var marker cacheComplete
	if err := json.NewDecoder(markerObject.Body).Decode(&marker); err != nil {
		t.Fatal(err)
	}
	_ = markerObject.Body.Close()
	marker.IntegrityState, marker.IntegrityDigest, marker.IntegrityMethod = "", "", ""
	marker.IntegritySize, marker.IntegrityVerifiedAt = 0, time.Time{}
	markerJSON, _ := json.Marshal(marker)
	if _, err := store.Store.Put(context.Background(), markerKey, bytes.NewReader(markerJSON), int64(len(markerJSON)), storage.PutOptions{IfMatch: markerObject.Version}); err != nil {
		t.Fatal(err)
	}

	inspection, err := cache.Inspect(context.Background(), "example.test", "team/image", digest, mediaType)
	if err != nil || !inspection.Unknown || inspection.Present {
		t.Fatalf("legacy inspection=%+v err=%v", inspection, err)
	}
	start := store.readBytes.Load()
	expected := CachedRepresentation{Authority: "example.test", Repository: "team/image", Digest: digest, MediaType: mediaType, Size: int64(len(data)), DataVersion: metadata.DataVersion}
	object, err := cache.Get(context.Background(), expected.Authority, expected.Repository, digest, mediaType)
	if err != nil {
		t.Fatal(err)
	}
	count, err := io.Copy(io.Discard, object.Body)
	closeErr := object.Body.Close()
	if err != nil || closeErr != nil || count != int64(len(data)) {
		t.Fatalf("stream count=%d err=%v close=%v", count, err, closeErr)
	}
	if got := store.readBytes.Load() - start; got != 2*int64(len(data)) {
		t.Fatalf("legacy verification plus normal stream read %d bytes, want %d", got, 2*len(data))
	}
	inspection, err = cache.Inspect(context.Background(), expected.Authority, expected.Repository, digest, mediaType)
	if err != nil || !inspection.Present || inspection.IntegrityMethod != "legacy_readback" {
		t.Fatalf("upgraded inspection=%+v err=%v", inspection, err)
	}
	object, err = cache.Get(context.Background(), expected.Authority, expected.Repository, digest, mediaType)
	if err != nil {
		t.Fatal(err)
	}
	count, err = io.Copy(io.Discard, object.Body)
	closeErr = object.Body.Close()
	if err != nil || closeErr != nil || count != int64(len(data)) {
		t.Fatalf("subsequent stream count=%d err=%v close=%v", count, err, closeErr)
	}
	if got := store.readBytes.Load() - start; got != 3*int64(len(data)) {
		t.Fatalf("subsequent trusted stream caused %d payload reads, want %d", got, 3*len(data))
	}
}

func TestInterruptedIntegrityProofUpgradeRequiresOneReadback(t *testing.T) {
	cache, store, digest, data, _ := payloadCacheFixture(t)
	identity := cacheIdentity("example.test", "team/image", digest)
	markerKey := cacheCompleteKey(identity, "application/octet-stream")
	markerObject, err := store.Store.Get(context.Background(), markerKey)
	if err != nil {
		t.Fatal(err)
	}
	var marker cacheComplete
	if err := json.NewDecoder(markerObject.Body).Decode(&marker); err != nil {
		t.Fatal(err)
	}
	_ = markerObject.Body.Close()
	marker.IntegrityDigest = "sha256:interrupted-proof"
	markerJSON, _ := json.Marshal(marker)
	if _, err := store.Store.Put(context.Background(), markerKey, bytes.NewReader(markerJSON), int64(len(markerJSON)), storage.PutOptions{IfMatch: markerObject.Version}); err != nil {
		t.Fatal(err)
	}
	inspection, err := cache.Inspect(context.Background(), "example.test", "team/image", digest, "application/octet-stream")
	if err != nil || !inspection.Unknown || inspection.Present {
		t.Fatalf("interrupted inspection=%+v err=%v", inspection, err)
	}
	start := store.readBytes.Load()
	object, err := cache.Get(context.Background(), "example.test", "team/image", digest, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	count, err := io.Copy(io.Discard, object.Body)
	closeErr := object.Body.Close()
	if err != nil || closeErr != nil || count != int64(len(data)) {
		t.Fatalf("readback stream count=%d err=%v close=%v", count, err, closeErr)
	}
	if got := store.readBytes.Load() - start; got != 2*int64(len(data)) {
		t.Fatalf("interrupted proof repair read %d bytes, want %d", got, 2*len(data))
	}
	inspection, err = cache.Inspect(context.Background(), "example.test", "team/image", digest, "application/octet-stream")
	if err != nil || !inspection.Present {
		t.Fatalf("repaired inspection=%+v err=%v", inspection, err)
	}
}

func TestCacheFallbackWriteStagesAndRejectsMismatchBeforePublication(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	cache := NewCache(&fallbackOnlyStore{Store: base})
	data := []byte("payload with an incorrect descriptor")
	wrongDigest := CalculateSHA256([]byte("different payload"))
	if err := cache.Put(context.Background(), "example.test", "team/image", wrongDigest, "application/octet-stream", bytes.NewReader(data), int64(len(data))); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Put error = %v, want digest mismatch", err)
	}
	entries, _, err := base.List(context.Background(), "cache/objects/", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed fallback write left cache objects: %+v", entries)
	}
}

type fallbackOnlyStore struct{ storage.Store }

type countedPayload struct {
	storage.PayloadReader
	store *payloadCountingStore
}

func (r countedPayload) Read(buffer []byte) (int, error) {
	if r.store.beforeRead != nil {
		r.store.beforeRead()
	}
	n, err := r.PayloadReader.Read(buffer)
	r.store.readBytes.Add(int64(n))
	return n, err
}

func (s *payloadCountingStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	payload, err := s.opener.OpenPayload(ctx, key)
	if err == nil {
		payload.Body = countedPayload{PayloadReader: payload.Body, store: s}
	}
	return payload, err
}

func payloadCacheFixture(t *testing.T) (*Cache, *payloadCountingStore, string, []byte, string) {
	t.Helper()
	root := t.TempDir()
	base, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	store := &payloadCountingStore{Store: base, opener: base, fingerprint: base}
	cache := NewCache(store)
	data := bytes.Repeat([]byte("verified payload\n"), 1<<16)
	digest := CalculateSHA256(data)
	if err := cache.Put(context.Background(), "example.test", "team/image", digest, "application/octet-stream", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	return cache, store, digest, data, root
}

func (s *payloadCountingStore) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	return s.fingerprint.Fingerprint(ctx, key)
}

func TestCachePayloadReadBounds(t *testing.T) {
	for _, mode := range []string{"verify", "consume", "partial"} {
		t.Run(mode, func(t *testing.T) {
			cache, store, digest, data, _ := payloadCacheFixture(t)
			want := int64(0)
			if mode == "verify" {
				if _, err := cache.VerifyAny(context.Background(), "example.test", "team/image", digest); err != nil {
					t.Fatal(err)
				}
				want = int64(len(data))
			} else {
				object, err := cache.GetAny(context.Background(), "example.test", "team/image", digest)
				if err != nil {
					t.Fatal(err)
				}
				var count int64
				if mode == "consume" {
					count, err = io.Copy(io.Discard, object.Body)
				} else {
					count, err = io.CopyN(io.Discard, object.Body, 17)
				}
				if err != nil {
					t.Fatal(err)
				}
				want += count
				if err := object.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if got := store.readBytes.Load(); got != want {
				t.Fatalf("payload bytes=%d, want %d", got, want)
			}
			pins, _, err := store.List(context.Background(), "cache/readers/", "", 100)
			if err != nil || len(pins) != 0 {
				t.Fatalf("leaked pins: %v, %v", pins, err)
			}
		})
	}
}
