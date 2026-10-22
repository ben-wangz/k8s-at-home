package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaySourceSpillsAndReopensCurrentInputRange(t *testing.T) {
	directory := t.TempDir()
	manager, err := newReplayManager(replayConfig{
		perSourceMemory: 4,
		maxMemory:       8,
		maxDisk:         64,
		directory:       directory,
	})
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	defer manager.Close()

	input := bytes.NewReader([]byte("prefix-payload"))
	if _, err := input.Seek(int64(len("prefix-")), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	source, err := manager.prepare(context.Background(), input, int64(len("payload")))
	if err != nil {
		t.Fatalf("prepare replay source: %v", err)
	}
	defer source.Close()
	if source.inMemory || source.path == "" {
		t.Fatal("large replay source did not spill to disk")
	}
	for attempt := 0; attempt < 2; attempt++ {
		reader, err := source.OpenRange(0, source.size)
		if err != nil {
			t.Fatalf("open replay attempt %d: %v", attempt+1, err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || string(data) != "payload" {
			t.Fatalf("replay attempt %d = %q, read error %v, close error %v", attempt+1, data, readErr, closeErr)
		}
	}
	if source.digest != sha256.Sum256([]byte("payload")) {
		t.Fatalf("logical source digest = %x", source.digest)
	}
	path := source.path
	source.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replay file still exists after release: %v", err)
	}
	if manager.diskUsed != 0 || manager.active != 0 {
		t.Fatalf("replay resources remain reserved: disk=%d active=%d", manager.diskUsed, manager.active)
	}
}

func TestReplaySourceAcceptsEmptyAndNonSeekableInputs(t *testing.T) {
	manager, err := newReplayManager(replayConfig{
		perSourceMemory: 2,
		maxMemory:       4,
		maxDisk:         16,
		directory:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	defer manager.Close()

	empty, err := manager.prepare(context.Background(), bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("prepare empty source: %v", err)
	}
	if empty.digest != sha256.Sum256(nil) {
		t.Fatalf("empty source digest = %x", empty.digest)
	}
	empty.Close()

	reader, writer := io.Pipe()
	writeResult := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(writer, "stream-data")
		writeResult <- writer.CloseWithError(writeErr)
	}()
	stream, err := manager.prepare(context.Background(), reader, int64(len("stream-data")))
	if err != nil {
		_ = reader.Close()
		t.Fatalf("prepare non-seekable source: %v", err)
	}
	_ = reader.Close()
	if err := <-writeResult; err != nil {
		t.Fatalf("write non-seekable source: %v", err)
	}
	defer stream.Close()
	opened, err := stream.OpenRange(0, stream.size)
	if err != nil {
		t.Fatalf("open non-seekable replay: %v", err)
	}
	data, readErr := io.ReadAll(opened)
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || string(data) != "stream-data" || stream.digest != sha256.Sum256(data) {
		t.Fatalf("non-seekable replay = %q, read error %v, close error %v", data, readErr, closeErr)
	}
}

func TestReplayManagerEnforcesConcurrentMemoryAndDiskBudgets(t *testing.T) {
	manager, err := newReplayManager(replayConfig{
		perSourceMemory: 4,
		maxMemory:       4,
		maxDisk:         5,
		directory:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	defer manager.Close()

	first, err := manager.prepare(context.Background(), strings.NewReader("1234"), 4)
	if err != nil {
		t.Fatalf("prepare in-memory source: %v", err)
	}
	second, err := manager.prepare(context.Background(), strings.NewReader("5678"), 4)
	if err != nil {
		t.Fatalf("prepare disk-backed source: %v", err)
	}
	if !first.inMemory || second.inMemory || manager.memoryUsed != 4 || manager.diskUsed != 4 {
		t.Fatalf("unexpected replay accounting: first memory=%t second memory=%t memory=%d disk=%d", first.inMemory, second.inMemory, manager.memoryUsed, manager.diskUsed)
	}
	memory, disk, active := manager.usage()
	if memory != 4 || disk != 4 || active != 2 {
		t.Fatalf("replay usage = (%d, %d, %d), want (4, 4, 2)", memory, disk, active)
	}
	if _, err := manager.prepare(context.Background(), strings.NewReader("abc"), 2); err == nil {
		t.Fatal("replay budget accepted data beyond its disk cap")
	}
	first.Close()
	second.Close()
	if manager.memoryUsed != 0 || manager.diskUsed != 0 || manager.active != 0 {
		t.Fatalf("replay budget did not release resources: memory=%d disk=%d active=%d", manager.memoryUsed, manager.diskUsed, manager.active)
	}
	memory, disk, active = manager.usage()
	if memory != 0 || disk != 0 || active != 0 {
		t.Fatalf("replay usage after release = (%d, %d, %d), want zero", memory, disk, active)
	}
}

func TestReplayManagerRejectsInvalidInputWithoutLeakingQuota(t *testing.T) {
	manager, err := newReplayManager(replayConfig{
		perSourceMemory: 8,
		maxMemory:       16,
		maxDisk:         32,
		directory:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create replay manager: %v", err)
	}
	defer manager.Close()

	for _, test := range []struct {
		name string
		body io.Reader
		size int64
	}{
		{name: "short", body: strings.NewReader("abc"), size: 4},
		{name: "long", body: strings.NewReader("abcde"), size: 4},
		{name: "zero with data", body: strings.NewReader("x"), size: 0},
		{name: "nil", body: nil, size: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := manager.prepare(context.Background(), test.body, test.size); err == nil {
				t.Fatal("invalid replay input was accepted")
			}
			if manager.memoryUsed != 0 || manager.diskUsed != 0 || manager.active != 0 {
				t.Fatalf("invalid input leaked replay quota: memory=%d disk=%d active=%d", manager.memoryUsed, manager.diskUsed, manager.active)
			}
		})
	}
}

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
