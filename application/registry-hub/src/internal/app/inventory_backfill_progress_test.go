package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryHistoricalBackfillResumesAndReadsEdgesLinearly(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	seedInventoryHistoricalGraphs(t, f)
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[]}`))
			return
		}
		f.serveHTTP(w, r)
	}))
	spy := &inventoryBackfillStore{Store: f.service.store, delay: time.Millisecond}
	f.service.store, f.service.inventory.Store = spy, spy
	ctx := context.Background()
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "backfill", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{Fence: leader.FencingToken}
	failed, unknown := false, false
	for n := 0; n < 20000; n++ {
		if root := state.Scan.Root; root != nil && root.Node != nil && len(root.Node.Content.Children) == 1 && !failed {
			spy.failCheckpoint, failed = true, true
		}
		if state.ImageBuild != nil && !unknown {
			spy.unknownCheckpoint, unknown = true, true
		}
		err := f.service.advanceInventory(ctx, leader, &state)
		if err != nil && !errors.Is(err, storage.ErrTimeout) {
			t.Fatal(err)
		}
		// Resume exclusively from committed state, including after an unknown commit.
		state, _, err = inventory.Read[inventoryCheckpoint](ctx, spy, inventory.Prefix+"checkpoint.json")
		if err != nil {
			t.Fatal(err)
		}
		if state.Build == nil && state.Scan.Complete {
			break
		}
		if n == 19999 {
			t.Fatal("historical backfill did not converge")
		}
	}
	if !failed || !unknown {
		t.Fatal("failure injection did not reach both source and snapshot construction")
	}
	if spy.edgeReads != 303 {
		t.Fatalf("edge GETs=%d, want exactly one per source edge", spy.edgeReads)
	}
	snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || snapshot.UniqueManifests != 104 || snapshot.Coverage != "complete" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	assertInventoryTestCounts(t, f.service, snapshot)
	contents, _, err := storage.Scan(ctx, spy, inventory.Prefix+"contents/", "", 1000)
	if err != nil || len(contents) != 104 {
		t.Fatalf("contents=%d err=%v", len(contents), err)
	}
	children := 0
	for _, entry := range contents {
		content, _, err := inventory.Read[inventory.Content](ctx, spy, entry.Key)
		if err != nil || !content.ClosureKnown {
			t.Fatalf("incomplete graph projection: %+v err=%v", content, err)
		}
		children += len(content.Children)
	}
	if children != 303 {
		t.Fatalf("children=%d, want all 303 edges without duplicates", children)
	}
	staging, _, err := storage.Scan(ctx, spy, inventory.Prefix+"projections/", "", 1)
	if err != nil || len(staging) != 0 {
		t.Fatalf("staging survived completed backfill: entries=%d err=%v", len(staging), err)
	}
}
