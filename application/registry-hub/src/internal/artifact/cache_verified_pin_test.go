package artifact

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

type fingerprintPayloadStore struct{ *payloadCountingStore }

func (s fingerprintPayloadStore) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	return s.Store.(storage.Fingerprinter).Fingerprint(ctx, key)
}

func pinnedProofFixture(t *testing.T) (*Cache, *payloadCountingStore, string, []byte, string) {
	t.Helper()
	cache, store, digest, data, root := payloadCacheFixture(t)
	cache.store = fingerprintPayloadStore{store}
	return cache, store, digest, data, root
}

func TestVerifiedPinChecksDoNotRereadPayloadAndRetainCleanupProtection(t *testing.T) {
	cache, store, digest, data, _ := pinnedProofFixture(t)
	ctx := context.Background()
	proof, err := cache.VerifyAnyPinned(ctx, "example.test", "team/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	for range 40 {
		if err := proof.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.readBytes.Load(); got != int64(len(data)) {
		t.Fatalf("verification plus checks read %d bytes, want %d", got, len(data))
	}
	identity := cacheIdentity("example.test", "team/image", digest)
	if err := cache.Remove(ctx, "example.test", "team/image", digest, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, "cache/objects/"+identity+"/data"); err != nil {
		t.Fatalf("active proof lost its payload: %v", err)
	}
	if err := proof.Check(ctx); !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("removed representation retained authority: %v", err)
	}
	if err := proof.Close(); err != nil {
		t.Fatal(err)
	}
	if err := proof.Check(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed proof was reusable: %v", err)
	}
	pins, _, err := store.List(ctx, "cache/readers/", "", 100)
	if err != nil || len(pins) != 0 {
		t.Fatalf("leaked pins=%v, error=%v", pins, err)
	}
}

func TestVerifiedPinRejectsChangedPayloadOrProtection(t *testing.T) {
	for _, mode := range []string{"missing", "replacement", "corruption", "marker", "index", "pin", "expiry", "renewal", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			cache, store, digest, data, root := pinnedProofFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			proof, err := cache.VerifyAnyPinned(ctx, "example.test", "team/image", digest)
			if err != nil {
				t.Fatal(err)
			}
			defer proof.Close()
			identity := cacheIdentity("example.test", "team/image", digest)
			path := filepath.Join(root, "cache/objects/"+identity+"/data")
			switch mode {
			case "missing":
				err = os.Remove(path)
			case "replacement":
				err = store.Store.(*filesystem.Store).Delete(ctx, "cache/objects/"+identity+"/data")
				if err == nil {
					_, err = store.Put(ctx, "cache/objects/"+identity+"/data", bytes.NewReader(data), int64(len(data)), storage.PutOptions{})
				}
			case "corruption":
				err = os.WriteFile(path, bytes.Repeat([]byte("x"), len(data)), 0600)
			case "marker":
				err = store.Delete(ctx, cacheCompleteKey(identity, proof.MediaType))
			case "index":
				err = store.Delete(ctx, cacheRepresentationIndexKey(identity))
			case "pin", "renewal":
				err = store.Delete(ctx, proof.reader.pin.key)
				if err == nil && mode == "renewal" && proof.reader.renewPin() == nil {
					t.Fatal("lost pin renewed successfully")
				}
				if mode == "renewal" && !errors.Is(proof.Context().Err(), context.Canceled) {
					t.Fatal("pin renewal failure did not cancel protected storage work")
				}
			case "expiry":
				proof.reader.mu.Lock()
				proof.reader.pin.expires = time.Now().Add(-time.Second)
				proof.reader.mu.Unlock()
			case "cancellation":
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := proof.Check(context.Background()); err == nil {
				t.Fatal("invalid proof authorized cleanup")
			}
		})
	}
}

func TestVerifiedPinRejectsCorruptionBeforeVerification(t *testing.T) {
	cache, _, digest, data, root := pinnedProofFixture(t)
	path := filepath.Join(root, "cache/objects/"+cacheIdentity("example.test", "team/image", digest)+"/data")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if proof, err := cache.VerifyAnyPinned(context.Background(), "example.test", "team/image", digest); err == nil {
		_ = proof.Close()
		t.Fatal("corrupt payload was accepted")
	}
}
