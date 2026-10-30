package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryFragmentRetentionPreservesPublishedSnapshot(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	key := inventoryFragmentKey(fragment.Image, "image")
	if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryFragment) error {
		value.CapturedAt = time.Now().Add(-2 * f.service.inventoryConfig().SnapshotRetention)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 100; step++ {
		done, err := f.service.cleanupInventoryFragments(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if step == 99 {
			t.Fatal("fragment retention did not converge")
		}
	}
	if _, err := f.service.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired fragment remains: %v", err)
	}
	image := inventoryTestImage(t, f.service, snapshot, fragment.Image.ID)
	if image.ContentID != fragment.Image.ContentID || image.Availability.State != "complete" {
		t.Fatalf("fragment retention changed immutable snapshot: %+v", image)
	}
	if _, err := f.service.store.Head(ctx, inventoryFragmentKey(fragment.Image, "content")); err != nil {
		t.Fatalf("fresh fragment was removed: %v", err)
	}
}
