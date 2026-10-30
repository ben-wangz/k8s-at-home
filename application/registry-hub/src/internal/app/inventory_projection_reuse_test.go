package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryUnchangedProjectionReuseAndInvalidation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	manifest, _, _ := publishAnchoredTestPlatform(f, "reuse-inventory")
	ctx := context.Background()
	items, _, err := f.service.tasks.List(ctx, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	rootID := ""
	for _, task := range items {
		if task.RootDigest == manifest.Digest {
			rootID = task.ID
		}
	}
	if rootID == "" {
		t.Fatal("root task missing")
	}
	spy := &inventoryBackfillStore{Store: f.service.store}
	f.service.store, f.service.inventory.Store = spy, spy
	started := time.Now()
	p, err := f.service.newInventoryProjection(ctx, rootID)
	if err != nil {
		t.Fatal(err)
	}
	for steps := 0; !p.Complete; steps++ {
		if steps > 500 {
			t.Fatal("cold projection did not converge")
		}
		if err := f.service.advanceInventoryProjection(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cold, reads := time.Since(started), spy.edgeReads
	spy.edgeReads = 0
	started = time.Now()
	warm, err := f.service.newInventoryProjection(ctx, rootID)
	if err != nil || !warm.Complete || spy.edgeReads != 0 {
		t.Fatalf("warm reuse=%+v edge reads=%d err=%v", warm, spy.edgeReads, err)
	}
	t.Logf("cold projection=%s (%d edge reads); unchanged projection=%s (zero edge reads)", cold, reads, time.Since(started))
	if err := inventory.Update(ctx, spy, projectionReuseKey(rootID), func(value *inventoryProjectionReceipt) error { value.Epoch++; return nil }); err != nil {
		t.Fatal(err)
	}
	changed, err := f.service.newInventoryProjection(ctx, rootID)
	if err != nil || changed.Complete {
		t.Fatal("GC epoch change reused stale projection")
	}
}

func TestInventoryClosureTopologyReuseRejectsChangedSourceVersion(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	configuration := f.cacheObject([]byte("closure configuration"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configuration, Layers: []artifact.Descriptor{}})
	root := f.root("closure-reuse", manifest)
	snapshot := refreshTestInventory(t, f.service)
	image := inventoryTestImage(t, f.service, snapshot, inventoryIdentity(root.Authority, root.Repository, root.Reference).ID())
	content, err := f.service.inventory.Content(ctx, image.Identity, image.ResolvedDigest)
	if err != nil {
		t.Fatal(err)
	}
	p, err := f.service.newInventoryImageProgress(ctx, image, content)
	if err != nil || p.ClosureReuse == nil {
		t.Fatalf("complete closure was not reusable: %+v %v", p, err)
	}
	key := inventory.Prefix + "contents/" + image.Identity.ContentID(image.ResolvedDigest) + ".json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *inventory.Content) error { value.Generation++; return nil }); err != nil {
		t.Fatal(err)
	}
	for steps := 0; p.ClosureReuse != nil && steps < 10; steps++ {
		if err := f.service.advanceClosureReuse(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if p.ClosureReuse != nil || p.TopologySaved || len(p.Closure.Frames) == 0 {
		t.Fatal("a changed source reused previously captured closure topology")
	}
}
