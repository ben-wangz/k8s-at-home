package app

import (
	"context"
	"fmt"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestInventoryRefreshLatencyWithStableContentAndChangingReference(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	var manifest artifact.Descriptor
	for n := 0; n < 4; n++ {
		manifest, _, _ = publishAnchoredTestPlatform(f, fmt.Sprintf("refresh-%d", n))
	}
	drainBackgroundWork(t, f.service, "inventory", "execution")
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
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "refresh-latency", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{Fence: leader.FencingToken}
	var previous inventory.Snapshot
	for round, target := range []time.Duration{60 * time.Second, 30 * time.Second} {
		started := time.Now()
		state.NextScanAt = time.Time{}
		spy.edgeReads = 0
		for step := 0; step < 10000; step++ {
			if step%25 == 0 {
				_, err := f.service.inventoryBinding(ctx, inventoryIdentity("ghcr.io", "team/image", "changing"), manifest.Digest, "upstream", 0, time.Now().UTC(), true)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.service.advanceInventory(ctx, leader, &state); err != nil {
				t.Fatal(err)
			}
			// A restarted worker must be able to continue after every committed step.
			state, _, err = inventory.Read[inventoryCheckpoint](ctx, spy, inventory.Prefix+"checkpoint.json")
			if err != nil {
				t.Fatal(err)
			}
			if state.Build == nil && state.Scan.Complete && state.LastCompleteAt.After(started) {
				break
			}
			if time.Since(started) > target || step == 9999 {
				t.Fatalf("round %d exceeded %s: build phase=%d steps=%d", round, target, state.BuildPhase, state.Steps)
			}
		}
		snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
		if err != nil || snapshot.Coverage != "complete" || snapshot.UniqueManifests != 4 || snapshot.SnapshotID == previous.SnapshotID {
			t.Fatalf("round %d snapshot=%+v err=%v", round, snapshot, err)
		}
		if round > 0 && spy.edgeReads != 0 {
			t.Fatalf("unchanged terminal graphs reread %d edges", spy.edgeReads)
		}
		t.Logf("round=%d elapsed=%s edgeReads=%d refreshSeconds=%.3f buildSeconds=%.3f", round, time.Since(started), spy.edgeReads, state.LastRefreshSeconds, state.LastBuildSeconds)
		previous = snapshot
	}
}
