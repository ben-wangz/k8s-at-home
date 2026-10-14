package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"registry-hub/internal/storage"
)

type interruptedReader struct {
	failed bool
}

func (r *interruptedReader) Read(buffer []byte) (int, error) {
	if !r.failed {
		r.failed = true
		return copy(buffer, "partial"), nil
	}
	return 0, errors.New("interrupted write")
}

func TestFailedPutKeepsPreviousObjectAndRemovesStagingFile(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	key := "objects/example"
	previous := []byte("complete")
	entry, err := store.Put(ctx, key, strings.NewReader(string(previous)), int64(len(previous)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, key, &interruptedReader{}, int64(len("partial replacement")), storage.PutOptions{IfMatch: entry.Version}); err == nil {
		t.Fatal("Put() accepted an interrupted stream")
	}

	object, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read existing object: read=%v close=%v", readErr, closeErr)
	}
	if string(data) != string(previous) {
		t.Fatalf("object after interrupted write = %q, want %q", data, previous)
	}

	path, err := store.path(key)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range entries {
		if strings.HasPrefix(item.Name(), ".staging-") {
			t.Fatalf("staging file %q remained after failed Put()", item.Name())
		}
	}
}

func TestPutFsyncAndRenameFailuresKeepPreviousObject(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*Store, error)
	}{
		{
			name: "file fsync",
			inject: func(store *Store, failure error) {
				store.ops.syncFile = func(*os.File) error { return failure }
			},
		},
		{
			name: "rename",
			inject: func(store *Store, failure error) {
				store.ops.rename = func(string, string) error { return failure }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			key := "objects/example"
			previous := []byte("committed-before-failure")
			entry, err := store.Put(ctx, key, strings.NewReader(string(previous)), int64(len(previous)), storage.PutOptions{IfAbsent: true})
			if err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected persistence failure")
			test.inject(store, failure)
			if _, err := store.Put(ctx, key, strings.NewReader("replacement"), int64(len("replacement")), storage.PutOptions{IfMatch: entry.Version}); !errors.Is(err, failure) {
				t.Fatalf("Put() error = %v, want injected failure", err)
			}
			if got := readObject(t, store, key); string(got) != string(previous) {
				t.Fatalf("object after failed replacement = %q, want %q", got, previous)
			}
			assertNoStagingFiles(t, store, key)
		})
	}
}

func TestPutDirectorySyncFailureReportsCommittedReplacement(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	key := "objects/example"
	previous := []byte("previous")
	entry, err := store.Put(ctx, key, strings.NewReader(string(previous)), int64(len(previous)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected directory fsync failure")
	store.ops.syncDir = func(string) error { return failure }
	if _, err := store.Put(ctx, key, strings.NewReader("replacement"), int64(len("replacement")), storage.PutOptions{IfMatch: entry.Version}); !errors.Is(err, failure) {
		t.Fatalf("Put() error = %v, want injected directory fsync failure", err)
	}
	if got := readObject(t, store, key); string(got) != "replacement" {
		t.Fatalf("object after post-rename failure = %q, want replacement", got)
	}
	assertNoStagingFiles(t, store, key)
}

func readObject(t *testing.T, store *Store, key string) []byte {
	t.Helper()
	object, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertNoStagingFiles(t *testing.T, store *Store, key string) {
	t.Helper()
	path, err := store.path(key)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".staging-") {
			t.Fatalf("staging file %q remained", entry.Name())
		}
	}
}

func TestFilesystemErrorsUseStorageClassifications(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want error
	}{
		{name: "permission", err: unix.EACCES, want: storage.ErrPermissionDenied},
		{name: "capacity", err: unix.ENOSPC, want: storage.ErrUnavailable},
		{name: "timeout", err: context.DeadlineExceeded, want: storage.ErrTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := mapError(test.err); !errors.Is(err, test.want) {
				t.Fatalf("mapError(%v) = %v, want errors.Is(_, %v)", test.err, err, test.want)
			}
		})
	}
	if err := mapError(storage.ErrConditionFailed); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("mapError(ErrConditionFailed) = %v, want the conditional error preserved", err)
	}
}
