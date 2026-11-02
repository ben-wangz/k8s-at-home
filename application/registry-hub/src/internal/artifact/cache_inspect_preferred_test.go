package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestInspectAnyIncludesPreferredMissingFromIndex(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cache, data := NewCache(store), []byte("preferred")
	digest, media := CalculateSHA256(data), "application/octet-stream"
	if err := cache.Put(ctx, "example.org", "repo", digest, media, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	key := cacheRepresentationIndexKey(cacheIdentity("example.org", "repo", digest))
	index, err := inspectJSON[cacheRepresentationIndex](ctx, store, key)
	if err != nil {
		t.Fatal(err)
	}
	index.MediaTypes = []string{"application/missing", "application/missing"}
	encoded, _ := json.Marshal(index)
	if _, err := store.Put(ctx, key, bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	guard := &inspectionGuard{Store: store, source: store}
	inspection, err := NewCache(guard).InspectAny(ctx, "example.org", "repo", digest, media)
	if err != nil || !inspection.Present || inspection.MediaType != media || guard.payloadReads != 0 || guard.writes != 0 {
		t.Fatalf("preferred=%+v err=%v", inspection, err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	inspection, err = NewCache(guard).InspectAny(ctx, "example.org", "repo", digest, media)
	if err != nil || !inspection.Present {
		t.Fatalf("index absent=%+v err=%v", inspection, err)
	}
}
