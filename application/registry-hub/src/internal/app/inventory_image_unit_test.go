package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryMetadataUnitCheckpointsBeforeImmutableCapture(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, 1)
	p := inventoryImageProgress{Phase: 6, Image: inventory.Image{ID: inventory.ID("unit"), Scope: "platform"}}
	done, err := f.service.advanceInventoryImageUnit(context.Background(), writer, &p)
	if err != nil || done || p.Phase != 7 || p.Capture.Phase != 0 {
		t.Fatalf("capture boundary was crossed before a durable checkpoint: phase=%d capture=%d done=%v error=%v", p.Phase, p.Capture.Phase, done, err)
	}
	key := inventory.Prefix + "snapshots/" + writer.Snapshot.SnapshotID + "/captures/" + p.Image.ID + ".json"
	if _, err := f.service.store.Head(context.Background(), key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("image capture started before checkpoint: %v", err)
	}
	if _, err := f.service.advanceInventoryImageUnit(context.Background(), writer, &p); err != nil || p.Capture.Phase != 1 {
		t.Fatalf("capture did not resume as one independently recoverable step: phase=%d error=%v", p.Capture.Phase, err)
	}
}
