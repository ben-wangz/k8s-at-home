package app

import (
	"context"
	"fmt"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestInventoryBatchYieldsBeforeDeadlineAndResumesRoot(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[]}`))
			return
		}
		f.serveHTTP(w, r)
	}))
	ctx := context.Background()
	digest := artifact.CalculateSHA256([]byte("budget-root"))
	root := f.root("budget", artifact.Descriptor{Digest: digest, Size: 1, MediaType: ociManifestMediaType})
	node := tasks.GraphNode{RootTaskID: root.ID, Role: tasks.GraphManifest, Digest: digest, MediaType: ociManifestMediaType, DeclaredSize: 1, ActualSize: 1, DiscoveryComplete: true}
	if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 50; n++ {
		child := artifact.CalculateSHA256([]byte(fmt.Sprint(n)))
		edge := tasks.DependencyEdge{RootTaskID: root.ID, ParentRole: tasks.GraphManifest, ParentDigest: digest, Order: n, ChildRole: tasks.GraphBlob, ChildDigest: child, Descriptor: tasks.GraphEntry{Digest: child, MediaType: "application/octet-stream", Size: 1}}
		if err := f.service.records.SaveDependencyEdge(ctx, edge); err != nil {
			t.Fatal(err)
		}
	}
	spy := &inventoryBackfillStore{Store: f.service.store, delay: 25 * time.Millisecond}
	observed := &observedStore{Store: spy, metrics: newMetrics()}
	f.service.store, f.service.inventory.Store = observed, observed
	f.service.cfg.Inventory = f.service.inventoryConfig()
	f.service.cfg.Inventory.BatchTimeout = time.Second
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "budget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{Fence: leader.FencingToken}
	for batch := 0; state.Scan.Phase == 0 && !state.Scan.Complete && batch < 20; batch++ {
		batchCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := f.service.runInventoryBatch(batchCtx, leader, &state)
		expired := batchCtx.Err()
		cancel()
		if err != nil || expired != nil {
			t.Fatalf("normal budget yield failed: err=%v context=%v", err, expired)
		}
		if batch == 0 && (state.Steps == 0 || state.Scan.Phase != 0 || state.Scan.Root == nil) {
			t.Fatal("large root did not commit partial progress before yielding")
		}
		state, _, err = inventory.Read[inventoryCheckpoint](ctx, spy, inventory.Prefix+"checkpoint.json")
		if err != nil {
			t.Fatal(err)
		}
		status, err := f.service.inventoryStatus(ctx)
		if err != nil || status.Batch.StartedAt.IsZero() || status.Batch.Steps == 0 || status.Batch.Steps > uint64(f.service.cfg.Inventory.BatchSize) || status.Batch.StorageOperations == 0 || status.Batch != state.Batch {
			t.Fatalf("committed batch diagnostics unavailable: status=%+v checkpoint=%+v err=%v", status.Batch, state.Batch, err)
		}
	}
	if state.Scan.Phase == 0 && !state.Scan.Complete || spy.edgeReads != 50 {
		t.Fatalf("root did not resume: phase=%d edge reads=%d", state.Scan.Phase, spy.edgeReads)
	}
}
