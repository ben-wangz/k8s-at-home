package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func setCacheTestSchemaVersion(t *testing.T, store storage.Store, key string, version int) {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record["schemaVersion"], err = json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version}); err != nil {
		t.Fatal(err)
	}
}

func TestGetAnyInvalidatesCorruptPayload(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	authority, repository := "ghcr.io", "team/image"
	body := []byte("verified payload")
	digest := CalculateSHA256(body)
	mediaType := "application/octet-stream"
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}

	identity := cacheIdentity(authority, repository, digest)
	dataKey := "cache/objects/" + identity + "/data"
	entry, err := store.Head(ctx, dataKey)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("corrupt payload")
	if _, err := store.Put(ctx, dataKey, bytes.NewReader(corrupt), int64(len(corrupt)), storage.PutOptions{IfMatch: entry.Version}); err != nil {
		t.Fatal(err)
	}

	if _, err := cache.GetAny(ctx, authority, repository, digest); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetAny() error = %v, want invalidated cache miss", err)
	}
	if _, err := store.Head(ctx, cacheMetadataKey(identity, mediaType)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("corrupt representation metadata remains active: %v", err)
	}
}

func cacheTestSchemaVersion(t *testing.T, store storage.Store, key string) int {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var record struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.NewDecoder(object.Body).Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record.SchemaVersion
}

func TestCacheReadsAndMigratesPreviousSchema(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("schema-migration-cache-content")
	digest := CalculateSHA256(body)
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	identity := cacheIdentity(authority, repository, digest)
	keys := []string{
		cacheMetadataKey(identity, mediaType),
		cacheCompleteKey(identity, mediaType),
		cacheRepresentationIndexKey(identity),
	}
	for _, key := range keys {
		setCacheTestSchemaVersion(t, store, key, cacheSchemaPrevious)
	}

	cached, err := cache.GetAny(ctx, authority, repository, digest)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(cached.Body)
	_ = cached.Body.Close()
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("migrated cache body = %q, err=%v", actual, err)
	}
	for _, key := range keys {
		if version := cacheTestSchemaVersion(t, store, key); version != cacheSchemaCurrent {
			t.Fatalf("schemaVersion for %q = %d, want %d", key, version, cacheSchemaCurrent)
		}
	}
}

func TestCacheDoesNotRewriteUnknownSchema(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("unknown-schema-cache-content")
	digest := CalculateSHA256(body)
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	key := cacheMetadataKey(cacheIdentity(authority, repository, digest), mediaType)
	setCacheTestSchemaVersion(t, store, key, 99)
	before, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Get(ctx, authority, repository, digest, mediaType); err == nil {
		t.Fatal("cache object with unknown schema was consumed")
	}
	after, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version || cacheTestSchemaVersion(t, store, key) != 99 {
		t.Fatal("cache object with unknown schema was changed")
	}
}
