package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestCacheRejectsUnknownCompleteAndIndexSchemasWithoutRewriting(t *testing.T) {
	for _, test := range []struct {
		name string
		key  func(string, string) string
		read func(context.Context, *Cache, string, string, string) error
	}{
		{
			name: "complete marker",
			key:  func(identity, mediaType string) string { return cacheCompleteKey(identity, mediaType) },
			read: func(ctx context.Context, cache *Cache, authority, repository, digest string) error {
				_, err := cache.Get(ctx, authority, repository, digest, "application/octet-stream")
				return err
			},
		},
		{
			name: "representation index",
			key:  func(identity, _ string) string { return cacheRepresentationIndexKey(identity) },
			read: func(ctx context.Context, cache *Cache, authority, repository, digest string) error {
				_, err := cache.GetAny(ctx, authority, repository, digest)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cache := NewCache(store)
			ctx := context.Background()
			body := []byte("unknown-schema-record-content")
			digest := CalculateSHA256(body)
			const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
			if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
				t.Fatal(err)
			}
			key := test.key(cacheIdentity(authority, repository, digest), mediaType)
			setCacheTestSchemaVersion(t, store, key, 99)
			before, err := store.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.read(ctx, cache, authority, repository, digest); err == nil {
				t.Fatal("cache consumed a record with an unsupported schema")
			}
			after, err := store.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if after.Version != before.Version || cacheTestSchemaVersion(t, store, key) != 99 {
				t.Fatal("cache changed a record with an unsupported schema")
			}
		})
	}
}

type listRejectingStore struct {
	storage.Store
	calls int
}

type deleteFailureStore struct {
	storage.Store
	failAt int
	calls  int
	err    error
}

type cleanupReferenceRaceStore struct {
	storage.Store
	key   string
	raced bool
}

func (s *deleteFailureStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.calls++
	if s.calls == s.failAt {
		return s.err
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func (s *cleanupReferenceRaceStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if key == s.key && !s.raced {
		s.raced = true
		object, err := s.Store.Get(ctx, key)
		if err != nil {
			return err
		}
		var metadata cacheMetadata
		err = json.NewDecoder(object.Body).Decode(&metadata)
		_ = object.Body.Close()
		if err != nil {
			return err
		}
		metadata.CleanupAfter = time.Now().Add(time.Hour)
		updated, err := json.Marshal(metadata)
		if err != nil {
			return err
		}
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version}); err != nil {
			return err
		}
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func (s *listRejectingStore) List(context.Context, string, string, int) ([]storage.Entry, string, error) {
	s.calls++
	return nil, "", errors.New("List must not be used by cache reads")
}
