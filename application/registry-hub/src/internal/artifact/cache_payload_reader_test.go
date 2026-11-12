package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheVerificationReleasesIdentityLockAndHonorsCancellation(t *testing.T) {
	cache, store, digest, _, _ := payloadCacheFixture(t)
	started, resume := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	store.beforeRead = func() {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-resume
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := cache.VerifyAny(ctx, "example.test", "team/image", digest); done <- err }()
	<-started
	locked := make(chan struct{})
	go func() {
		unlock := cache.lockIdentity(cacheIdentity("example.test", "team/image", digest))
		unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		cancel()
		close(resume)
		<-done
		t.Fatal("full verification retained the striped identity lock")
	}
	cancel()
	close(resume)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled verification=%v", err)
	}
	pins, _, err := store.List(context.Background(), "cache/readers/", "", 100)
	if err != nil || len(pins) != 0 {
		t.Fatalf("cancel leaked pins: %v, %v", pins, err)
	}
}

func TestVerifiedCacheReaderStaysOnOriginalDescriptor(t *testing.T) {
	cache, _, digest, data, root := payloadCacheFixture(t)
	object, err := cache.GetAny(context.Background(), "example.test", "team/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	path := filepath.Join(root, "cache/objects/"+cacheIdentity("example.test", "team/image", digest)+"/data")
	if err := os.WriteFile(path+".replacement", bytes.Repeat([]byte("x"), len(data)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(object.Body)
	if !bytes.Equal(actual, data) || err != nil && !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("reader switched to replacement payload: bytes=%d, error=%v", len(actual), err)
	}
}

type replacedConsumptionStore struct {
	storage.Store
	gets int
}

func (s *replacedConsumptionStore) Get(ctx context.Context, key string) (storage.Object, error) {
	object, err := s.Store.Get(ctx, key)
	if err == nil && filepath.Base(key) == "data" {
		s.gets++
		if s.gets == 1 {
			object.Version = "different-object"
		}
	}
	return object, err
}

func TestCacheRejectsChangedConsumptionIdentity(t *testing.T) {
	cache, store, digest, _, _ := payloadCacheFixture(t)
	cache = NewCache(&replacedConsumptionStore{Store: store.Store})
	if object, err := cache.Get(context.Background(), "example.test", "team/image", digest, "application/octet-stream"); !errors.Is(err, storage.ErrCorruptObject) {
		if object.Body != nil {
			_ = object.Body.Close()
		}
		t.Fatalf("changed consumption identity was exposed: %v", err)
	}
}
