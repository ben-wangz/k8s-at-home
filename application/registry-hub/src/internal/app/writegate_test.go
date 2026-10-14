package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
)

func TestWriteGateDrainRejectsNewWritesAndWaitsForAcceptedWrites(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	release, err := gate.Enter(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("new write admission during drain = %v, want ErrWritesClosed", err)
	}
	if err := gate.WaitQuiesced(ctx, 5*time.Millisecond); err == nil {
		t.Fatal("drain completed while an accepted write remained active")
	}
	release()
	if err := gate.WaitQuiesced(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	if state := gate.State().State; state != "backend_quiesced" {
		t.Fatalf("gate state after drain = %q, want backend_quiesced", state)
	}
}

func TestFailedUnknownWriteGateStaysClosedAfterRestart(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := gate.WaitQuiesced(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := gate.SetGCState(ctx, "failed_unknown", "local:test", "executor_unknown"); err != nil {
		t.Fatal(err)
	}

	restarted, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if state := restarted.State().State; state != "failed_unknown" {
		t.Fatalf("restarted gate state = %q, want failed_unknown", state)
	}
	if _, err := restarted.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("write admission after restart = %v, want ErrWritesClosed", err)
	}
	if err := restarted.Open(ctx); err == nil {
		t.Fatal("failed_unknown gate opened without recovery")
	}
	controller := &gcController{
		service: &Service{cfg: config.Config{GCMode: "disabled"}, gate: restarted},
		run:     gcRun{ID: "pending", Mode: "disabled", State: "failed_unknown"},
	}
	if _, err := controller.Retry(ctx, false); err == nil {
		t.Fatal("GC retry proceeded without explicit no-executor confirmation")
	}
	if _, err := controller.Recover(ctx, false); err == nil {
		t.Fatal("GC recovery proceeded without explicit no-executor confirmation")
	}
	if state := restarted.State().State; state != "failed_unknown" {
		t.Fatalf("gate state after refused retry or recovery = %q, want failed_unknown", state)
	}
}
