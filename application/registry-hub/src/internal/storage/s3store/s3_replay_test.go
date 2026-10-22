package s3store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
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
