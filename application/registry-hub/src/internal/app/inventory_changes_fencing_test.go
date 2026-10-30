package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryGraphMarkersCoalesceAndPreserveNewerRevision(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	root := ""
	for _, task := range fragment.Image.Solidification.Tasks {
		if task.Kind == "root" {
			root = task.ID
		}
	}
	if root == "" {
		t.Fatal("fixture omitted its root task")
	}
	key := "solidification/graphs/" + root + "/"
	for _, suffix := range []string{"nodes/one.json", "edges/two.json"} {
		if err := f.service.recordInventoryChange(ctx, key+suffix, "raw-source-version"); err != nil {
			t.Fatal(err)
		}
	}
	change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
	if err != nil || change.Key != key || change.SourceVersion != "" {
		t.Fatalf("graph updates were not coalesced: %+v error=%v", change, err)
	}
	p, err := f.service.newInventoryProjection(ctx, root)
	if err != nil || p.Complete {
		t.Fatalf("graph change reused a task-only terminal receipt: projection=%+v error=%v", p, err)
	}
	if err := f.service.recordInventoryChange(ctx, key+"nodes/newer.json", "newer-source-version"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.acknowledgeInventoryProjection(ctx, p); err != nil {
		t.Fatal(err)
	}
	current, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
	if err != nil || current.Revision == change.Revision || current.Revision == "" {
		t.Fatalf("old projection retired newer graph work: %+v error=%v", current, err)
	}
}

func TestInventoryStaleOwnerCannotReplaceDirtyRevisionOrFragment(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	change := inventoryChange{Key: inventory.Prefix + "proofs/" + fragment.Image.ContentID + ".json", Revision: "newer", SourceVersion: "changed"}
	if err := f.service.invalidateInventoryFragment(ctx, fragment, []inventoryChange{change}, 20); err != nil {
		t.Fatal(err)
	}
	change.Revision = "older"
	if err := f.service.invalidateInventoryFragment(ctx, fragment, []inventoryChange{change}, 19); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("old owner replaced newer dirty work: %v", err)
	}
	dirty, _, err := inventory.Read[inventoryDirtyEntity](ctx, f.service.store, inventoryDirtyKey(fragment.Image.ID))
	if err != nil {
		t.Fatal(err)
	}
	p := inventoryImageProgress{Image: fragment.Image, Objects: fragment.Objects, Closure: fragment.Closure, Dependencies: fragment.Dependencies, Epoch: fragment.Epoch, DirtyRevision: dirty.Revision, Fence: 19}
	if err := f.service.acknowledgeInventoryImage(ctx, &p); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("old capture acknowledged newer ownership: %v", err)
	}
	p.Fence = 20
	if err := f.service.saveInventoryFragment(ctx, &p, "image"); err != nil {
		t.Fatal(err)
	}
	p.Fence = 19
	p.Image.Evidence.CheckedAt = p.Image.Evidence.CheckedAt.Add(time.Second)
	if err := f.service.saveInventoryFragment(ctx, &p, "image"); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("old owner replaced a newer fragment using its timestamp: %v", err)
	}
}
