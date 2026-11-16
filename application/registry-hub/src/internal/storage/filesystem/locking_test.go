package filesystem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"registry-hub/internal/storage"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestKeyChurnBoundsLocksAndReclaimsEmptyDirectories(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < lockStripes+128; i++ {
		key := fmt.Sprintf("temporary/%d/record.json", i)
		entry, err := store.Put(ctx, key, strings.NewReader("data"), 4, storage.PutOptions{IfAbsent: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteIfMatch(ctx, key, entry.Version); err != nil {
			t.Fatal(err)
		}
	}
	locks, directories := 0, 0
	err = filepath.WalkDir(store.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories++
		} else {
			if filepath.Dir(path) != filepath.Join(store.root, lockDirectory) {
				t.Errorf("churn left an object or legacy lock: %s", path)
			}
			locks++
		}
		return nil
	})
	if err != nil || locks == 0 || locks > lockStripes || directories != 2 {
		t.Fatalf("locks=%d directories=%d err=%v", locks, directories, err)
	}
	for _, list := range []func(context.Context, string, string, int) ([]storage.Entry, string, error){store.List, store.ListMetadata} {
		entries, next, err := list(ctx, "", "", 10)
		if err != nil || len(entries) != 0 || next != "" {
			t.Fatalf("internal pool leaked into listing: %+v %s %v", entries, next, err)
		}
	}
	if _, err := store.Put(ctx, lockDirectory+"/data", strings.NewReader("x"), 1, storage.PutOptions{}); !errors.Is(err, storage.ErrInvalidKey) {
		t.Fatalf("internal key accepted: %v", err)
	}
}

func TestRootMaintenanceLockBlocksMutationsWithoutCreatingDirectories(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := unix.Flock(int(root.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := store.Put(ctx, "must/not/appear", strings.NewReader("x"), 1, storage.PutOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("mutation bypassed maintenance: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.root, "must")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("waiting writer created directories: %v", err)
	}
}

func TestIndependentProcessesRetainConditionalWriteExclusion(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "counter", strings.NewReader("0"), 1, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	commands := make([]*exec.Cmd, 4)
	for i := range commands {
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFilesystemCounterProcess$")
		command.Env = append(os.Environ(), "REGISTRY_HUB_COUNTER_ROOT="+store.root)
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		commands[i] = command
	}
	for _, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(readObject(t, store, "counter")); got != "80" {
		t.Fatalf("cross-process updates were lost: %s", got)
	}
}

func TestFilesystemCounterProcess(t *testing.T) {
	root := os.Getenv("REGISTRY_HUB_COUNTER_ROOT")
	if root == "" {
		return
	}
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; {
		object, err := store.Get(context.Background(), "counter")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(object.Body)
		_ = object.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		body, err := strconv.Atoi(string(data))
		if err != nil {
			t.Fatal(err)
		}
		value := strconv.Itoa(body + 1)
		_, err = store.Put(context.Background(), "counter", strings.NewReader(value), int64(len(value)), storage.PutOptions{IfMatch: object.Version})
		if errors.Is(err, storage.ErrConditionFailed) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		i++
	}
}
