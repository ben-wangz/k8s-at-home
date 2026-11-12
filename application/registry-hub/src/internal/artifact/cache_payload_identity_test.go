package artifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestCachePayloadSharesVersionHashOnlyForSHA256(t *testing.T) {
	for _, algorithm := range []string{"sha256", "sha384", "sha512"} {
		t.Run(algorithm, func(t *testing.T) {
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			store := &payloadCountingStore{Store: base, opener: base, fingerprint: base}
			cache := NewCache(store)
			data := bytes.Repeat([]byte("digest and version"), 4096)
			hash, _ := Hasher(algorithm + ":" + strings.Repeat("0", 128))
			_, _ = hash.Write(data)
			digest := algorithm + ":" + hex.EncodeToString(hash.Sum(nil))
			if err := cache.Put(context.Background(), "example.test", "image", digest, "application/octet-stream", bytes.NewReader(data), int64(len(data))); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.VerifyAny(context.Background(), "example.test", "image", digest); err != nil {
				t.Fatal(err)
			}
			if count := store.readBytes.Load(); count != int64(len(data)) {
				t.Fatalf("dual digest computation reread the payload: %d", count)
			}
		})
	}
}

type mutateVerifiedPayload struct {
	storage.PayloadReader
	mutate func()
}

func (r mutateVerifiedPayload) Check(ctx context.Context) error {
	r.mutate()
	return r.PayloadReader.Check(ctx)
}

type mutationPayloadStore struct {
	*filesystem.Store
	mutate func()
}

func (s mutationPayloadStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	payload, err := s.Store.OpenPayload(ctx, key)
	if err == nil {
		payload.Body = mutateVerifiedPayload{PayloadReader: payload.Body, mutate: s.mutate}
	}
	return payload, err
}

func TestCachePayloadRejectsInPlaceMutationAfterHashing(t *testing.T) {
	cache, store, digest, data, root := payloadCacheFixture(t)
	path := filepath.Join(root, "cache/objects/"+cacheIdentity("example.test", "team/image", digest)+"/data")
	cache.store = mutationPayloadStore{Store: store.Store.(*filesystem.Store), mutate: func() {
		changed := bytes.Repeat([]byte("x"), len(data))
		if err := os.WriteFile(path, changed, 0600); err != nil {
			t.Fatal(err)
		}
	}}
	object, err := cache.Get(context.Background(), "example.test", "team/image", digest, "application/octet-stream")
	if object.Body != nil {
		_, _ = io.Copy(io.Discard, object.Body)
		_ = object.Body.Close()
		t.Fatal("mutated descriptor was exposed")
	}
	if !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("mutation after verification was not rejected: %v", err)
	}
}
