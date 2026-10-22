package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestCacheCleanupPendingRemainsReadableAndCanRestore(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("shared-cleanup-pending-content")
	digest := CalculateSHA256(body)
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/vnd.oci.image.manifest.v1+json"
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := cache.MarkCleanupPending(ctx, authority, repository, []string{digest}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Get(ctx, authority, repository, digest, mediaType)
	if err != nil {
		t.Fatalf("cleanup-pending Get error = %v", err)
	}
	actual, err := io.ReadAll(cached.Body)
	_ = cached.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("cleanup-pending cache body = %q, err=%v", actual, err)
	}
	retained, err := cache.GetRetained(ctx, authority, repository, digest, mediaType)
	if err != nil {
		t.Fatalf("cleanup-pending retained Get error = %v", err)
	}
	actual, err = io.ReadAll(retained.Body)
	_ = retained.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("retained cache body = %q, err=%v", actual, err)
	}
	retained, err = cache.GetAnyRetained(ctx, authority, repository, digest)
	if err != nil {
		t.Fatalf("cleanup-pending retained GetAny error = %v", err)
	}
	actual, err = io.ReadAll(retained.Body)
	_ = retained.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("retained GetAny body = %q, err=%v", actual, err)
	}
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	cached, err = cache.Get(ctx, authority, repository, digest, mediaType)
	if err != nil {
		t.Fatalf("restored cache Get error = %v", err)
	}
	actual, err = io.ReadAll(cached.Body)
	_ = cached.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("restored cache body = %q, err=%v", actual, err)
	}
}

func TestCacheCleanupPendingRemainsReadableAfterEarliestReclaimTime(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("expired-cleanup-pending-content")
	digest := CalculateSHA256(body)
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/vnd.oci.image.manifest.v1+json"
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := cache.MarkCleanupPending(ctx, authority, repository, []string{digest}, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	retained, err := cache.GetRetained(ctx, authority, repository, digest, mediaType)
	if err != nil {
		t.Fatalf("retained Get after earliest reclaim time error = %v", err)
	}
	actual, err := io.ReadAll(retained.Body)
	_ = retained.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("retained body after earliest reclaim time = %q, err=%v", actual, err)
	}
	retained, err = cache.GetAnyRetained(ctx, authority, repository, digest)
	if err != nil {
		t.Fatalf("retained GetAny after earliest reclaim time error = %v", err)
	}
	actual, err = io.ReadAll(retained.Body)
	_ = retained.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("retained GetAny body after earliest reclaim time = %q, err=%v", actual, err)
	}
	if err := cache.Remove(ctx, authority, repository, digest, mediaType); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetRetained(ctx, authority, repository, digest, mediaType); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("retained Get after physical removal error = %v, want ErrNotFound", err)
	}
}

func TestCacheIncompleteCommitCannotBeConsumed(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("uncommitted-cache-content")
	digest := CalculateSHA256(body)
	commitFailure := errors.New("stop before complete marker")
	err = cache.PutChecked(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body)), func() error {
		return commitFailure
	})
	if !errors.Is(err, commitFailure) {
		t.Fatalf("PutChecked error = %v, want interrupted commit", err)
	}

	identity := cacheIdentity("ghcr.io", "team/image", digest)
	for _, key := range []string{
		"cache/objects/" + identity + "/data",
		cacheMetadataKey(identity, "application/octet-stream"),
	} {
		if _, err := store.Head(ctx, key); err != nil {
			t.Fatalf("expected pre-complete object %q to exist: %v", key, err)
		}
	}
	if _, err := store.Head(ctx, cacheCompleteKey(identity, "application/octet-stream")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("complete marker error = %v, want not found", err)
	}
	if cached, err := cache.Get(ctx, "ghcr.io", "team/image", digest, "application/octet-stream"); !errors.Is(err, storage.ErrNotFound) {
		if err == nil {
			_ = cached.Body.Close()
		}
		t.Fatalf("incomplete cache object was consumable: %v", err)
	}
}

func TestCacheGetAnyDoesNotListRepresentations(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	guarded := &listRejectingStore{Store: store}
	cache := NewCache(guarded)
	ctx := context.Background()
	body := []byte("indexed-cache-content")
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.GetAny(ctx, "ghcr.io", "team/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Body.Close()
	actual, err := io.ReadAll(cached.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, body) {
		t.Fatalf("cached body = %q, want %q", actual, body)
	}
	if guarded.calls != 0 {
		t.Fatalf("List calls = %d, want 0", guarded.calls)
	}
}

func TestCacheRecoveryRebuildsMissingRepresentationIndex(t *testing.T) {
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cache := NewCache(store)
	body := []byte("legacy-cache-content")
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, "application/octet-stream", bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	identity := cacheIdentity("ghcr.io", "team/image", digest)
	if err := store.Delete(ctx, cacheRepresentationIndexKey(identity)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered := NewCache(reopened)
	if err := recovered.RebuildRepresentationIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	cached, err := recovered.GetAny(ctx, "ghcr.io", "team/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer cached.Body.Close()
	actual, err := io.ReadAll(cached.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, body) {
		t.Fatalf("recovered body = %q, want %q", actual, body)
	}
}

func TestCacheGetAnyRejectsIncompleteIndexedRepresentation(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("incomplete-indexed-content")
	digest := CalculateSHA256(body)
	mediaType := "application/octet-stream"
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	identity := cacheIdentity("ghcr.io", "team/image", digest)
	if err := store.Delete(ctx, cacheCompleteKey(identity, mediaType)); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetAny(ctx, "ghcr.io", "team/image", digest); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("GetAny error = %v, want ErrUnavailable", err)
	}
}
