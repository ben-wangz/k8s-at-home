package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestGCFaultLoggingUsesStableErrorClasses(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	secretDetails := "s3-secret-key signed-url=https://bucket.example/?signature=private"
	executable := filepath.Join(t.TempDir(), "gc-failure")
	script := "#!/bin/sh\nprintf '%s\\n' '" + secretDetails + "'\nexit 1\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	controller := &gcController{
		service: &Service{
			cfg:   config.Config{GCExecutable: executable, GCConfig: "/private/registry-config.yml"},
			store: store, gate: gate, metrics: newMetrics(),
			logger: slog.New(slog.NewJSONHandler(&output, nil)),
		},
		run: gcRun{SchemaVersion: 1, ID: "0123456789abcdef0123456789abcdef", Mode: "local", State: "gc_running"},
	}
	if err := controller.runLocal(ctx); err == nil {
		t.Fatal("GC executable failure was not returned")
	}
	controller.fail("executor_failed", "failed")
	if strings.Contains(output.String(), secretDetails) || strings.Contains(output.String(), "/private/registry-config.yml") {
		t.Fatalf("GC fault log exposed command output or configuration path: %s", output.String())
	}
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode GC fault log %q: %v", line, err)
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0]["error_class"] != "gc_executor" || records[1]["error_class"] != "executor_failed" {
		t.Fatalf("GC fault log classes = %#v, want gc_executor and executor_failed", records)
	}
}

func (s *gcCompletionStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if s.failRunWrite && key == "system/gc/last-run.json" {
		return storage.Entry{}, storage.ErrUnavailable
	}
	if s.verifyOpenAfterState && key == "system/gc/state.json" {
		var state gateRecord
		if err := json.Unmarshal(data, &state); err != nil {
			return storage.Entry{}, err
		}
		if state.State == "open" {
			object, err := s.Store.Get(ctx, "system/gc/last-run.json")
			if err != nil {
				return storage.Entry{}, err
			}
			var run gcRun
			err = json.NewDecoder(object.Body).Decode(&run)
			_ = object.Body.Close()
			if err != nil {
				return storage.Entry{}, err
			}
			if run.State != s.expectedOpenState {
				return storage.Entry{}, errors.New("GC write gate opened before terminal state was persisted")
			}
			s.openedAfterState = true
		}
	}
	return s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
}

func newGCCompletionTestController(t *testing.T, store storage.Store) (*gcController, *writeGate) {
	t.Helper()
	ctx := context.Background()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.SetGCState(ctx, "restore", "local:test", ""); err != nil {
		t.Fatal(err)
	}
	controller := &gcController{
		service: &Service{store: store, gate: gate},
		run:     gcRun{SchemaVersion: 1, ID: strings.Repeat("a", 32), State: "gc_running", ExecutorID: "local:test"},
	}
	controller.mu.Lock()
	err = controller.persistLocked(ctx)
	controller.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return controller, gate
}

func TestGCOpensWriteGateOnlyAfterSuccessStateIsPersisted(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &gcCompletionStore{Store: base}
	controller, gate := newGCCompletionTestController(t, store)
	store.verifyOpenAfterState = true
	store.expectedOpenState = "succeeded"
	if err := controller.complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.openedAfterState || gate.State().State != "open" {
		t.Fatalf("openedAfterState=%t gateState=%q", store.openedAfterState, gate.State().State)
	}
}

func TestGCCompletionStorageFailureLeavesWriteGateClosed(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &gcCompletionStore{Store: base}
	controller, gate := newGCCompletionTestController(t, store)
	store.failRunWrite = true
	if err := controller.complete(context.Background()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("complete error = %v, want storage unavailable", err)
	}
	if gate.State().State != "restore" {
		t.Fatalf("write gate state after completion failure = %q, want restore", gate.State().State)
	}
}

func TestGCRecoveryOpensWriteGateOnlyAfterRecoveredStateIsPersisted(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &gcCompletionStore{Store: base}
	controller, gate := newGCCompletionTestController(t, store)
	store.verifyOpenAfterState = true
	store.expectedOpenState = "recovered"
	if err := controller.completeWithState(context.Background(), "recovered"); err != nil {
		t.Fatal(err)
	}
	if !store.openedAfterState || gate.State().State != "open" {
		t.Fatalf("openedAfterState=%t gateState=%q", store.openedAfterState, gate.State().State)
	}
}

func TestGCRecoveryStateStorageFailureLeavesWriteGateClosed(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &gcCompletionStore{Store: base}
	controller, gate := newGCCompletionTestController(t, store)
	store.failRunWrite = true
	if err := controller.completeWithState(context.Background(), "recovered"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("recovery completion error = %v, want storage unavailable", err)
	}
	if gate.State().State != "restore" {
		t.Fatalf("write gate state after recovery state failure = %q, want restore", gate.State().State)
	}
}

func TestGCRunReplacementPreservesStoreVersion(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	controller := &gcController{
		service: &Service{store: store},
		run: gcRun{
			SchemaVersion: 1,
			ID:            strings.Repeat("a", 32),
			Mode:          "local",
			State:         "failed",
			StartedAt:     time.Now().UTC(),
		},
	}
	ctx := context.Background()
	controller.mu.Lock()
	err = controller.persistLocked(ctx)
	controller.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	controller.mu.Lock()
	controller.run = newGCRun(controller.run, config.Config{GCMode: "local"}, strings.Repeat("b", 32), time.Now().UTC())
	err = controller.persistLocked(ctx)
	controller.mu.Unlock()
	if err != nil {
		t.Fatalf("persist replacement run: %v", err)
	}

	object, err := store.Get(ctx, "system/gc/last-run.json")
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var persisted gcRun
	if err := json.NewDecoder(object.Body).Decode(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.ID != strings.Repeat("b", 32) || persisted.State != "draining" {
		t.Fatalf("persisted retry run = (%q, %q), want replacement ID in draining state", persisted.ID, persisted.State)
	}
}
