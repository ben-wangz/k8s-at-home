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
	checkpoint := p
	if _, err := f.service.advanceInventoryImageUnit(context.Background(), writer, &p); err != nil || p.Capture.Phase != 4 {
		t.Fatalf("capture did not resume as a bounded unit: phase=%d error=%v", p.Capture.Phase, err)
	}
	// A lost outer checkpoint replays immutable payloads from the frozen metadata.
	p = checkpoint
	if _, err := f.service.advanceInventoryImageUnit(context.Background(), writer, &p); err != nil || p.Capture.Phase != 4 {
		t.Fatalf("capture replay changed immutable payloads: phase=%d error=%v", p.Capture.Phase, err)
	}
	if ready, err := writer.HasImage(context.Background(), p.Image.ID); err != nil || ready {
		t.Fatalf("capture published a ready marker before its bounded payload unit completed: %v", err)
	}
	if done, err := f.service.advanceInventoryImageUnit(context.Background(), writer, &p); err != nil || !done {
		t.Fatalf("replayed capture did not complete: done=%v error=%v", done, err)
	}
}
