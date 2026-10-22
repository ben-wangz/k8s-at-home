package tasks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

type taskCreateFaultStore struct {
	storage.Store
	put func(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.Entry, error, bool)
}

func (s *taskCreateFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if entry, err, handled := s.put(ctx, key, body, size, options); handled {
		return entry, err
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestEnqueueResolvedCleansProtectionReferenceWhenTaskCreateFails(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &taskCreateFaultStore{Store: base, put: func(_ context.Context, key string, _ io.Reader, _ int64, _ storage.PutOptions) (storage.Entry, error, bool) {
		if strings.HasPrefix(key, "tasks/state/") {
			return storage.Entry{}, storage.ErrUnavailable, true
		}
		return storage.Entry{}, nil, false
	}}
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	_, _, err = manager.EnqueueResolved(context.Background(), "ghcr.io", "team/image", "latest", testRootDigest(), "application/vnd.oci.image.manifest.v1+json", 1)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("EnqueueResolved() error = %v, want storage unavailable", err)
	}
	assertNoTaskCreationRecords(t, base)
}

func TestEnqueueResolvedCleansProtectionReferenceWhenInflightWriteFails(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &taskCreateFaultStore{Store: base, put: func(_ context.Context, key string, _ io.Reader, _ int64, _ storage.PutOptions) (storage.Entry, error, bool) {
		if strings.HasPrefix(key, "tasks/inflight/") {
			return storage.Entry{}, storage.ErrConditionFailed, true
		}
		return storage.Entry{}, nil, false
	}}
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	_, _, err = manager.EnqueueResolved(context.Background(), "ghcr.io", "team/image", "latest", testRootDigest(), "application/vnd.oci.image.manifest.v1+json", 1)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("EnqueueResolved() error = %v, want storage unavailable", err)
	}
	assertNoTaskCreationRecords(t, base)
}

func TestEnqueueResolvedConfirmsUnknownInflightCommitByReadback(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &taskCreateFaultStore{Store: base, put: func(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error, bool) {
		if !strings.HasPrefix(key, "tasks/inflight/") {
			return storage.Entry{}, nil, false
		}
		entry, err := base.Put(ctx, key, bytes.NewReader(readAll(t, body)), size, options)
		if err != nil {
			return entry, err, true
		}
		return entry, storage.ErrUnknownCommit, true
	}}
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()

	task, created, err := manager.EnqueueResolved(context.Background(), "ghcr.io", "team/image", "latest", testRootDigest(), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil || !created || task.ID == "" {
		t.Fatalf("EnqueueResolved() = (%#v, %t, %v), want confirmed task", task, created, err)
	}
	if _, err := manager.Get(context.Background(), task.ID); err != nil {
		t.Fatalf("confirmed task lookup error = %v", err)
	}
	references, _, err := NewRecords(base).ListCacheProtectionReferences(context.Background(), testRootDigest(), "", 10)
	if err != nil || len(references) != 1 || references[0].TaskID != task.ID {
		t.Fatalf("cache protection references = %#v, %v; want the confirmed task", references, err)
	}
}

func readAll(t *testing.T, reader io.Reader) []byte {
	t.Helper()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertNoTaskCreationRecords(t *testing.T, store storage.Store) {
	t.Helper()
	for _, prefix := range []string{"tasks/state/", "solidification/cache-protection/"} {
		entries, _, err := store.List(context.Background(), prefix, "", 100)
		if err != nil {
			t.Fatalf("List(%q) error = %v", prefix, err)
		}
		if len(entries) != 0 {
			t.Fatalf("records under %q = %d, want none", prefix, len(entries))
		}
	}
}

func testRootDigest() string {
	return "sha256:" + strings.Repeat("a", 64)
}
