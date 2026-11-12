package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestCleanupUsesKnownVersionWithoutRedundantPayloadHead(t *testing.T) {
	for _, mode := range []string{"matching", "replacement", "missing", "unknown version"} {
		t.Run(mode, func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			key := "cache/objects/cleanup-test/data"
			entry, err := store.Put(ctx, key, strings.NewReader("original"), 8, storage.PutOptions{})
			if err != nil {
				t.Fatal(err)
			}
			version := entry.Version
			if mode == "replacement" {
				if _, err := store.Put(ctx, key, strings.NewReader("replacement"), 11, storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			} else if mode == "missing" {
				if err := store.Delete(ctx, key); err != nil {
					t.Fatal(err)
				}
			} else if mode == "unknown version" {
				version = ""
			}
			var hashed int64
			ctx = storage.WithIOObserver(ctx, func(_ context.Context, operation string, count int64, _ time.Duration) {
				if operation == "hash" {
					hashed += count
				}
			})
			if err := NewCache(store).removeDataIfUnreferenced(ctx, "cleanup-test", version); err != nil {
				t.Fatal(err)
			}
			want := int64(8)
			if mode == "replacement" {
				want = 11
			} else if mode == "missing" {
				want = 0
			} else if mode == "unknown version" {
				want = 16
			}
			if hashed != want {
				t.Fatalf("cleanup hashed %d bytes, want %d", hashed, want)
			}
			_, err = store.Head(context.Background(), key)
			if mode == "replacement" && err != nil || mode != "replacement" && !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("unexpected payload deletion: %v", err)
			}
		})
	}
}
