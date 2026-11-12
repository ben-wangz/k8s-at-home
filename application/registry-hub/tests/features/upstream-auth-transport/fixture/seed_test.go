package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSeedTaskRecordsProjectedInputsAndExistingState(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	projected := filepath.Join(source, "..data")
	if err := os.Mkdir(projected, 0750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"new.json": `{"id":"new"}`, "existing.json": `{"id":"seed"}`} {
		if err := os.WriteFile(filepath.Join(projected, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(source, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "ignored.txt"), []byte("ignored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing.json"), []byte(`{"id":"original"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := seedTaskRecords(source, destination); err != nil {
			t.Fatal(err)
		}
	}
	for name, expected := range map[string]string{"new.json": `{"id":"new"}`, "existing.json": `{"id":"original"}`} {
		content, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || string(content) != expected {
			t.Fatalf("%s: content=%q, error=%v", name, content, err)
		}
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 2 {
		t.Fatalf("unexpected state files or temporary files: %v, %v", entries, err)
	}
}

func TestSeedTaskRecordsPropagatesReadFailure(t *testing.T) {
	source, destination := t.TempDir(), t.TempDir()
	if err := os.Symlink("missing", filepath.Join(source, "broken.json")); err != nil {
		t.Fatal(err)
	}
	if err := seedTaskRecords(source, destination); err == nil {
		t.Fatal("unreadable task record was silently ignored")
	}
	if err := seedTaskRecords(filepath.Join(source, "missing"), destination); err == nil {
		t.Fatal("missing seed directory was silently ignored")
	}
}

func TestSeedTaskRecordsPropagatesDestinationFailure(t *testing.T) {
	source, destination := t.TempDir(), filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(destination, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := seedTaskRecords(source, destination); err == nil {
		t.Fatal("invalid destination was silently ignored")
	}
}
