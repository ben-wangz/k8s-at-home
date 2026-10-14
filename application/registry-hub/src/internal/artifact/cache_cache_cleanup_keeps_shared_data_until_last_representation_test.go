package artifact

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestCacheCleanupKeepsSharedDataUntilLastRepresentation(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("cache-content")
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, "ghcr.io", "org/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := cache.Put(ctx, "ghcr.io", "org/image", digest, "application/vnd.test+json", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	removed, err := cache.Cleanup(ctx, time.Now().Add(48*time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("first cleanup removed = %d, want 1", removed)
	}
	dataKey := "cache/objects/" + cacheIdentity("ghcr.io", "org/image", digest) + "/data"
	if _, err := store.Head(ctx, dataKey); err != nil {
		t.Fatalf("shared cache data was removed before its last representation: %v", err)
	}
	removed, err = cache.Cleanup(ctx, time.Now().Add(48*time.Hour), 100)
	if err != nil || removed != 1 {
		t.Fatalf("second cleanup removed=%d err=%v, want 1", removed, err)
	}
	if _, err := store.Head(ctx, dataKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("last cleanup left cache data or returned wrong error: %v", err)
	}
}

func TestCacheCleanupDoesNotDeleteRepresentationRefreshedAfterRead(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	body := []byte("concurrently-refreshed-cache-content")
	digest := CalculateSHA256(body)
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
	if err := NewCache(store).Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	identity := cacheIdentity(authority, repository, digest)
	metadataKey := cacheMetadataKey(identity, mediaType)
	tracingStore := &cleanupReferenceRaceStore{Store: store, key: metadataKey}
	cache := NewCache(tracingStore)
	removed, err := cache.Cleanup(ctx, time.Now().Add(48*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || !tracingStore.raced {
		t.Fatalf("cleanup removed=%d raced=%t, want no removal after a concurrent reference refresh", removed, tracingStore.raced)
	}
	cached, err := cache.Get(ctx, authority, repository, digest, mediaType)
	if err != nil {
		t.Fatalf("refreshed cache reference was not consumable: %v", err)
	}
	actual, err := io.ReadAll(cached.Body)
	_ = cached.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("refreshed cache body = %q, err=%v", actual, err)
	}
}

func TestCacheCleanupRetainsObjectWhenReferenceStateIsUnavailable(t *testing.T) {
	for _, failure := range []error{storage.ErrUnavailable, storage.ErrCorruptObject} {
		t.Run(failure.Error(), func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cache := NewCache(store)
			ctx := context.Background()
			body := []byte("protected-cache-content")
			digest := CalculateSHA256(body)
			if err := cache.Put(ctx, "ghcr.io", "org/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatal(err)
			}
			cache.SetOrphanProtection(func(context.Context, string, string, string) (bool, error) {
				return false, failure
			})

			removed, err := cache.Cleanup(ctx, time.Now().Add(48*time.Hour), 100)
			if !errors.Is(err, failure) || removed != 0 {
				t.Fatalf("cleanup removed=%d err=%v, want no removal and %v", removed, err, failure)
			}
			cached, err := cache.Get(ctx, "ghcr.io", "org/image", digest, "application/octet-stream")
			if err != nil {
				t.Fatalf("cache object was not retained after uncertain reference state: %v", err)
			}
			_ = cached.Body.Close()
		})
	}
}

func TestCacheCleanupInterruptionRetainsDataAndCanRecover(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("delete-stage-%d", failAt), func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			body := []byte("cleanup-interruption-content")
			digest := CalculateSHA256(body)
			const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
			if err := NewCache(store).Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected cleanup delete failure")
			failingStore := &deleteFailureStore{Store: store, failAt: failAt, err: failure}
			cache := NewCache(failingStore)
			now := time.Now().Add(48 * time.Hour)
			if _, err := cache.Cleanup(ctx, now, 100); !errors.Is(err, failure) {
				t.Fatalf("Cleanup() error = %v, want injected delete failure at stage %d", err, failAt)
			}
			dataKey := "cache/objects/" + cacheIdentity(authority, repository, digest) + "/data"
			object, err := store.Get(ctx, dataKey)
			if err != nil {
				t.Fatalf("cleanup failure at stage %d removed shared data: %v", failAt, err)
			}
			actual, readErr := io.ReadAll(object.Body)
			_ = object.Body.Close()
			if readErr != nil || !bytes.Equal(actual, body) {
				t.Fatalf("data after cleanup failure = %q, err=%v", actual, readErr)
			}
			if _, err := cache.Cleanup(ctx, now, 100); err != nil {
				t.Fatalf("retry cleanup: %v", err)
			}
			if _, err := cache.CleanupOrphans(ctx, now, 24*time.Hour, 100); err != nil {
				t.Fatalf("orphan recovery: %v", err)
			}
			if _, err := store.Head(ctx, dataKey); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("recovery left cache data or returned wrong error: %v", err)
			}
		})
	}
}

func TestCacheCleanupPendingUsesExplicitDeadline(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("pending-content")
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, "docker.io", "library/test", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Hour)
	if err := cache.MarkCleanupPending(ctx, "docker.io", "library/test", []string{digest}, deadline); err != nil {
		t.Fatal(err)
	}
	if removed, err := cache.Cleanup(ctx, time.Now().Add(time.Hour), 100); err != nil || removed != 0 {
		t.Fatalf("early cleanup removed=%d err=%v", removed, err)
	}
	if removed, err := cache.Cleanup(ctx, deadline.Add(time.Second), 100); err != nil || removed != 1 {
		t.Fatalf("expired cleanup removed=%d err=%v", removed, err)
	}
}

func TestCacheStatsReportCommittedPendingStagingAndOrphanObjects(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("verified-cache-object")
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := cache.MarkCleanupPending(ctx, "ghcr.io", "team/image", []string{digest}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "cache/objects/staging/data", bytes.NewReader([]byte("staging")), 7, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "cache/objects/orphan/data", bytes.NewReader([]byte("orphan")), 6, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	stats, err := cache.Stats(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Objects != 1 || stats.Bytes != int64(len(body)) || stats.CleanupPending != 1 || stats.Staging != 2 || stats.Orphans != 0 {
		t.Fatalf("recent cache stats = %#v, want one verified pending object and two staging objects", stats)
	}
	stats, err = cache.Stats(ctx, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Objects != 1 || stats.Bytes != int64(len(body)) || stats.CleanupPending != 1 || stats.Staging != 0 || stats.Orphans != 2 {
		t.Fatalf("stale cache stats = %#v, want one verified pending object and two orphans", stats)
	}
}
