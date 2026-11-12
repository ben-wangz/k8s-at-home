package artifact

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"sync/atomic"
	"testing"
	"time"
)

type generatedLayer struct{ offset uint64 }

func (r *generatedLayer) Read(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = byte((r.offset*31 + r.offset/251) % 256)
		r.offset++
	}
	return len(buffer), nil
}

type legacyPayloadStore struct {
	storage.Store
	readBytes atomic.Int64
}

func (s *legacyPayloadStore) payloadContext(ctx context.Context, key string) context.Context {
	if filepath.Base(key) != "data" {
		return ctx
	}
	return storage.WithIOObserver(ctx, func(_ context.Context, operation string, count int64, _ time.Duration) {
		if operation == "read" {
			s.readBytes.Add(count)
		}
	})
}

func (s *legacyPayloadStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	return s.Store.Head(s.payloadContext(ctx, key), key)
}

func (s *legacyPayloadStore) Get(ctx context.Context, key string) (storage.Object, error) {
	ctx = s.payloadContext(ctx, key)
	object, err := s.Store.Get(ctx, key)
	if err == nil {
		object.Body = storage.MeasureBody(ctx, object.Body)
	}
	return object, err
}

func TestLargeLayerPayloadAmplification(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_LARGE_LAYER_TEST") != "1" {
		t.Skip("set REGISTRY_HUB_LARGE_LAYER_TEST=1 for generated 64/256/1024 MiB layers")
	}
	for _, mib := range []int64{64, 256, 1024} {
		t.Run(fmt.Sprintf("%dMiB", mib), func(t *testing.T) {
			measureLargeLayer(t, mib<<20)
		})
	}
}

func measureLargeLayer(t *testing.T, size int64) {
	t.Helper()
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &payloadCountingStore{Store: base, opener: base, fingerprint: base}
	cache := NewCache(store)
	hash, _ := Hasher("sha256:" + fmt.Sprintf("%064d", 0))
	if _, err := io.CopyN(hash, &generatedLayer{}, size); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if err := cache.Put(ctx, "example.test", "large/image", digest, "application/octet-stream", io.LimitReader(&generatedLayer{}, size), size); err != nil {
		t.Fatal(err)
	}
	legacy := &legacyPayloadStore{Store: base}
	started := time.Now()
	// Reproduce the historical eager Head, verification Get, and consumption Get.
	if _, err := legacy.Head(ctx, "cache/objects/"+cacheIdentity("example.test", "large/image", digest)+"/data"); err != nil {
		t.Fatal(err)
	}
	consumeLargeLayer(t, NewCache(legacy), digest, size)
	baseline := time.Since(started)
	if count := legacy.readBytes.Load(); count != 5*size {
		t.Fatalf("historical reads=%d, want %d", count, 5*size)
	}
	started = time.Now()
	consumeLargeLayer(t, cache, digest, size)
	consumption := time.Since(started)
	if count := store.readBytes.Swap(0); count != 2*size {
		t.Fatalf("optimized reads=%d, want %d", count, 2*size)
	}
	started = time.Now()
	if _, err := cache.VerifyAny(ctx, "example.test", "large/image", digest); err != nil {
		t.Fatal(err)
	}
	verification := time.Since(started)
	if count := store.readBytes.Load(); count != size {
		t.Fatalf("verify-only reads=%d, want %d", count, size)
	}
	t.Logf("warm local filesystem: bytes=%d baseline_reads=%d optimized_reads=%d verify_reads=%d baseline=%s optimized=%s verify=%s", size, 5*size, 2*size, size, baseline, consumption, verification)
}

func consumeLargeLayer(t *testing.T, cache *Cache, digest string, size int64) {
	t.Helper()
	object, err := cache.GetAny(context.Background(), "example.test", "large/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	count, readErr := io.Copy(io.Discard, object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || count != size {
		t.Fatalf("consume count=%d, read=%v, close=%v", count, readErr, closeErr)
	}
}
