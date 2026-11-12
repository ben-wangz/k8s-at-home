package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"testing"
	"time"
)

type gatedPayload struct {
	io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedPayload) Read(body []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.Reader.Read(body)
}

func blockedStaging(t *testing.T, store *Store) {
	t.Helper()
	body := &gatedPayload{Reader: strings.NewReader("replacement"), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := store.Put(context.Background(), "cache/layer", body, 11, storage.PutOptions{})
		done <- err
	}()
	t.Cleanup(func() {
		close(body.release)
		if err := <-done; err != nil {
			t.Errorf("finish payload: %v", err)
		}
	})
	select {
	case <-body.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("payload did not reach staging")
	}
}

func TestBlockedPayloadDoesNotBlockUnrelatedCASOrDeletes(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entry, err := store.Put(ctx, "metadata/lease", strings.NewReader("old"), 3, storage.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blockedStaging(t, store)
	finished := make(chan error, 1)
	go func() {
		latest, err := store.Put(ctx, "metadata/lease", strings.NewReader("renewed"), 7, storage.PutOptions{IfMatch: entry.Version})
		if err == nil {
			err = store.DeleteIfMatch(ctx, "metadata/lease", latest.Version)
		}
		if err == nil {
			_, err = store.Put(ctx, "metadata/another", strings.NewReader("another"), 7, storage.PutOptions{IfAbsent: true})
		}
		if err == nil {
			err = store.Delete(ctx, "metadata/another")
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated lease metadata waited for payload staging")
	}
}

func TestBlockedPayloadRetainsSameKeyExclusion(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blockedStaging(t, store)
	other, err := New(store.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, writer := range []*Store{store, other} {
		for _, operation := range []string{"put", "delete", "delete_match"} {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			switch operation {
			case "put":
				_, err = writer.Put(ctx, "cache/layer", strings.NewReader("lost"), 4, storage.PutOptions{})
			case "delete":
				err = writer.Delete(ctx, "cache/layer")
			case "delete_match":
				err = writer.DeleteIfMatch(ctx, "cache/layer", "stale")
			}
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("same-key mutation bypassed payload lock: %v", err)
			}
		}
	}
}

func TestOrphanCleanupSkipsActiveStagingAndCollectsUnrelatedFiles(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blockedStaging(t, store)
	old := time.Now().Add(-2 * time.Hour)
	store.mu.Lock()
	for path := range store.staging {
		if err := os.Chtimes(path, old, old); err != nil {
			store.mu.Unlock()
			t.Fatal(err)
		}
	}
	store.mu.Unlock()
	orphan := filepath.Join(store.root, ".staging-orphan")
	if err := os.WriteFile(orphan, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := store.CleanupOrphans(context.Background(), time.Hour, 10)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for path := range store.staging {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("active staging disappeared: %v", err)
		}
	}
}
