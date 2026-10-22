package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

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
