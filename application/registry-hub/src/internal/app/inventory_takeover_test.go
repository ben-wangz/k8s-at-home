package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryLeaderTakeoverPreservesBuildAndFencesPublication(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	identity := inventoryIdentity("ghcr.io", "team/image", "latest")
	config := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	f.root("latest", manifest)
	oldSnapshot := refreshTestInventory(t, f.service)
	first, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, first.FencingToken)
	gate, err := f.service.inventoryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer.Snapshot.GCEpoch = gate.Epoch
	state := inventoryCheckpoint{Fence: first.FencingToken, Build: &writer.Snapshot, Scan: inventoryScan{Complete: true}}
	for n := 0; n < 1000; n++ {
		if err := f.service.advanceInventory(ctx, first, &state); err != nil {
			t.Fatal(err)
		}
		if state.ImageBuild != nil && state.ImageBuild.Offset > 0 {
			break
		}
	}
	if state.ImageBuild == nil || state.ImageBuild.Offset == 0 {
		t.Fatal("did not capture partial image progress")
	}
	id, offset := state.Build.SnapshotID, state.ImageBuild.Offset
	if err := f.service.tasks.Leases().Release(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "second", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, second)
	if err := f.service.claimInventory(ctx, second, &state); err != nil {
		t.Fatal(err)
	}
	if state.Build.SnapshotID != id || state.ImageBuild.Offset != offset || state.Build.BuilderFence != second.FencingToken {
		t.Fatal("takeover discarded partial capture")
	}
	oldWriter := f.service.inventory.ResumeWriter(oldSnapshot)
	if err := oldWriter.Publish(ctx, true, nil); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("stale publication was not fenced: %v", err)
	}
	for n := 0; state.Build != nil && n < 2000; n++ {
		if err := f.service.advanceInventory(ctx, second, &state); err != nil {
			t.Fatal(err)
		}
	}
	if state.Build != nil {
		t.Fatal("resumed build did not complete")
	}
	snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || snapshot.SnapshotID != id {
		t.Fatalf("published snapshot=%s err=%v", snapshot.SnapshotID, err)
	}
	inventoryTestImage(t, f.service, snapshot, identity.ID())
}
