package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryMaintenanceProcessesBatchAndProtectsActiveBuild(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	current := inventory.Snapshot{SnapshotID: inventory.ID("snapshot", "current"), ExpiresAt: time.Now().Add(time.Hour)}
	active := inventory.Snapshot{SnapshotID: inventory.ID("snapshot", "active"), ExpiresAt: time.Now().Add(-time.Hour)}
	expired := inventory.Snapshot{SnapshotID: inventory.ID("snapshot", "expired"), ExpiresAt: time.Now().Add(-time.Hour)}
	for _, snapshot := range []inventory.Snapshot{current, active, expired} {
		key := inventory.Prefix + "snapshots/" + snapshot.SnapshotID + "/manifest.json"
		if err := f.service.inventory.Immutable(ctx, key, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	for n := 0; n < 10; n++ {
		key := inventory.Prefix + "snapshots/" + expired.SnapshotID + fmt.Sprintf("/entries/%02d.json", n)
		if err := f.service.inventory.Immutable(ctx, key, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"current.json", current); err != nil {
		t.Fatal(err)
	}
	state := inventoryCheckpoint{Build: &active}
	if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"checkpoint.json", state); err != nil {
		t.Fatal(err)
	}
	f.service.maintainInventory(ctx, &state)
	entries, _, err := storage.Scan(ctx, f.service.store, inventory.Prefix+"snapshots/"+expired.SnapshotID+"/", "", 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("maintenance failed to drain its bounded batch: entries=%d err=%v", len(entries), err)
	}
	for _, snapshot := range []inventory.Snapshot{current, active} {
		if _, err := f.service.store.Head(ctx, inventory.Prefix+"snapshots/"+snapshot.SnapshotID+"/manifest.json"); err != nil {
			t.Fatalf("protected snapshot removed: %v", err)
		}
	}
}
