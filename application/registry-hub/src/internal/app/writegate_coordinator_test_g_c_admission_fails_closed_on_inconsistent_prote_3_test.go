package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestGCAdmissionFailsClosedOnInconsistentProtectionLedger(t *testing.T) {
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
	gate.state.LegacyGrandfatheredBytes = 1
	before := gate.State()
	if err := gate.BeginDrain(ctx); err == nil {
		t.Fatal("GC admission accepted an inconsistent legacy reservation total")
	}
	if state := gate.State(); state.State != before.State || state.Epoch != before.Epoch {
		t.Fatalf("gate after rejected GC admission = %#v, want unchanged open state %#v", state, before)
	}
}

func mustGateTestID(t *testing.T) string {
	t.Helper()
	id, err := randomRunID()
	if err != nil {
		t.Fatal(err)
	}
	if !validGateID(id) || strings.TrimSpace(id) != id {
		t.Fatalf("generated invalid gate ID %q", id)
	}
	return id
}

func assertGateStateSchema(t *testing.T, store *filesystem.Store, expected int) {
	t.Helper()
	object, err := store.Get(context.Background(), "system/gc/state.json")
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var state struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.NewDecoder(object.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != expected {
		t.Fatalf("gate schema version = %d, want %d", state.SchemaVersion, expected)
	}
}
