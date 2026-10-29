package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryPlaceholderTakeoverRetainsProgressAndFencesOldRefresh(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	first, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	writer := f.service.inventory.NewWriter(time.Now().Add(-2*time.Hour).UTC(), time.Hour, first.FencingToken)
	state := inventoryCheckpoint{Fence: first.FencingToken, Build: &writer.Snapshot, Steps: 67, BuildPhase: 2, BuildToken: "retained-token"}
	if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"checkpoint.json", state); err != nil {
		t.Fatal(err)
	}
	if err := f.service.refreshInventoryPlaceholder(ctx, first, &state, &inventoryAvailability{}); err != nil {
		t.Fatal(err)
	}
	old, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error { value.ExpiresAt = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.service.refreshInventoryPlaceholder(ctx, second, &state, &inventoryAvailability{}); err != nil {
		t.Fatal(err)
	}
	current, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || current.SnapshotID == old.SnapshotID || current.BuilderFence != second.FencingToken || !current.BuildStartedAt.Before(state.Build.BuildStartedAt) || state.Build.SnapshotID != writer.Snapshot.SnapshotID || state.Steps != 67 || state.BuildPhase != 2 || state.BuildToken != "retained-token" {
		t.Fatalf("takeover changed progress or publication order: state=%+v current=%+v err=%v", state, current, err)
	}
	if err := f.service.inventory.ResumeWriter(old).Publish(ctx, false, nil); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("stale placeholder publication was not fenced: %v", err)
	}
}
