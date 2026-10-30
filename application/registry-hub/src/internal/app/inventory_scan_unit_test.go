package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
)

func TestInventorySourceUnitCheckpointsBeforeImmutableGraphCapture(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	manifest := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("source-unit")), MediaType: ociManifestMediaType, Size: 1}
	task := f.root("source-unit", manifest)
	node := tasks.GraphNode{RootTaskID: task.ID, Role: tasks.GraphManifest, Digest: manifest.Digest, MediaType: manifest.MediaType, DeclaredSize: 1, ActualSize: 1, DiscoveryComplete: true}
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	state := inventoryScan{}
	if err := f.service.scanInventoryUnit(ctx, &state); err != nil {
		t.Fatal(err)
	}
	if state.Root == nil || state.Root.Prefix == "" || state.Root.Phase != 0 {
		t.Fatalf("source capture identity was not yielded before use: %+v", state.Root)
	}
	prefix := state.Root.Prefix
	entries, _, err := storage.Scan(ctx, f.service.store, prefix, "", 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("graph pages written before their capture identity was checkpointed: entries=%d error=%v", len(entries), err)
	}
	key := inventory.Prefix + "source-unit-test.json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryScan) error { *value = state; return nil }); err != nil {
		t.Fatal(err)
	}
	for replay := 0; replay < 2; replay++ {
		state, _, err = inventory.Read[inventoryScan](ctx, f.service.store, key)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.scanInventoryUnit(ctx, &state); err != nil {
			t.Fatalf("capture replay %d failed: %v", replay, err)
		}
		entries, _, err = storage.Scan(ctx, f.service.store, prefix+"nodes/", "", 2)
		if err != nil || len(entries) != 1 || state.Root == nil || state.Root.Prefix != prefix {
			t.Fatalf("capture did not preserve its durable identity: entries=%d error=%v", len(entries), err)
		}
	}
}
