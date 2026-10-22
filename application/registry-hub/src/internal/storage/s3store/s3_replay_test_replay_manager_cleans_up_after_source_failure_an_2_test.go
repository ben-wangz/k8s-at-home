package s3store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestReplayManagerCleansUpAfterSourceFailureAndCancellation(t *testing.T) {
	directory := t.TempDir()
	manager, err := newReplayManager(replayConfig{
		perSourceMemory: 1,
		maxMemory:       2,
		maxDisk:         32,
		directory:       directory,
	})
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	defer manager.Close()

	readFailure := errors.New("source read failed")
	if _, err := manager.prepare(context.Background(), replayErrorReader{err: readFailure}, 8); !errors.Is(err, readFailure) {
		t.Fatalf("prepare with source failure = %v, want wrapped source error", err)
	}
	assertReplayResourcesReleased(t, manager, directory)

	ctx, cancel := context.WithCancel(context.Background())
	reader := replayCancelReader{cancel: cancel}
	if _, err := manager.prepare(ctx, &reader, 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare with cancelled source = %v, want context.Canceled", err)
	}
	assertReplayResourcesReleased(t, manager, directory)
}

func assertReplayResourcesReleased(t *testing.T, manager *replayManager, directory string) {
	t.Helper()
	if manager.memoryUsed != 0 || manager.diskUsed != 0 || manager.active != 0 {
		t.Fatalf("replay resources remain reserved: memory=%d disk=%d active=%d", manager.memoryUsed, manager.diskUsed, manager.active)
	}
	files, err := filepath.Glob(filepath.Join(directory, replayFilePrefix+"*.tmp"))
	if err != nil {
		t.Fatalf("find replay files: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("replay files remain after failure: %v", files)
	}
}

type replayErrorReader struct{ err error }

func (r replayErrorReader) Read([]byte) (int, error) { return 0, r.err }

type replayCancelReader struct {
	cancel context.CancelFunc
	done   bool
}

func (r *replayCancelReader) Read(data []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(data, []byte("part"))
	r.cancel()
	return n, nil
}

func TestReplayManagerOwnsDirectoryLockAndRemovesOnlyReplayFiles(t *testing.T) {
	directory := t.TempDir()
	stale := filepath.Join(directory, replayFilePrefix+"stale.tmp")
	other := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	config := replayConfig{directory: directory}
	manager, err := newReplayManager(config)
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale replay file was not removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
	if _, err := newReplayManager(config); err == nil {
		t.Fatal("a second manager acquired an active replay directory")
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("close replay manager: %v", err)
	}
	second, err := newReplayManager(config)
	if err != nil {
		t.Fatalf("reopen released replay directory: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close second replay manager: %v", err)
	}
}
