package artifact

import (
	"bytes"
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestCacheCleanupOrphansRemovesAbandonedFallbackStaging(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	key := "cache/objects/fallback-identity/staging/temporary"
	entry, err := store.Put(ctx, key, bytes.NewReader([]byte("staged")), 6, storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := NewCache(store).CleanupOrphans(ctx, time.Now().Add(48*time.Hour), time.Hour, 10)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v, want one abandoned staging object", removed, err)
	}
	if _, err := store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("staging object remains: %v (entry version %q)", err, entry.Version)
	}
}
