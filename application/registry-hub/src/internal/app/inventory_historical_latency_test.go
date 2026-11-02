package app

import (
	"context"
	"fmt"
	"os"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage/filesystem"
	"sync/atomic"
	"testing"
	"time"
)

func TestInventoryHistoricalRefreshWithConcurrentMaintenance(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_TEST_INVENTORY_SCALE") != "1" {
		t.Skip("set REGISTRY_HUB_TEST_INVENTORY_SCALE=1 for historical acceptance")
	}
	local, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	store := &inventoryLatencyStore{Store: local, delay: time.Millisecond}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	enableInventory(f)
	seedInventoryHistoricalGraphs(t, f)
	// The source-filter fixture also contains deliberately invalid blob-task records.
	for n := range 50 {
		if err := store.Delete(context.Background(), fmt.Sprintf("tasks/state/%032x.json", n+1)); err != nil {
			t.Fatal(err)
		}
	}
	f.service.backgroundSlots, f.service.collectorSlots = make(chan struct{}, 2), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var completed atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.runPeriodicMaintenance(ctx, "latency_fixture", func(ctx context.Context) (maintenanceProgress, error) {
			err := inventory.Update(ctx, store, "fixture/maintenance.json", func(count *int) error { *count++; return nil })
			if err == nil {
				completed.Add(1)
			}
			return maintenanceProgress{EntityProgress: err == nil, RoundComplete: err == nil}, err
		})
	}()
	defer func() { cancel(); <-done }()
	verifyInventoryStorageLatency(t, f, store, 20, 104)
	if completed.Load() < 2 {
		t.Fatal("ordinary maintenance did not progress alongside historical inventory")
	}
	t.Logf("historical roots=20 nodes=322 edges=303; ordinary completed=%d", completed.Load())
}
