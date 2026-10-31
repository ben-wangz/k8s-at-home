package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

type inventoryMaintenanceStore struct {
	storage.Store
	delay    time.Duration
	fail     bool
	timeouts int
}

func (s *inventoryMaintenanceStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.fail {
		return storage.Object{}, storage.ErrTimeout
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		s.timeouts++
		return storage.Object{}, ctx.Err()
	case <-timer.C:
		return s.Store.Get(ctx, key)
	}
}

func inventoryEventCount(t *testing.T, s *Service, event string) float64 {
	t.Helper()
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "registry_hub_events_total" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetValue() == event {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func TestInventoryMaintenanceYieldsBeforeStartingAnotherStorageUnit(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	expired := inventory.Snapshot{SnapshotID: inventory.ID("expired"), ExpiresAt: time.Now().Add(-time.Hour)}
	if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"snapshots/"+expired.SnapshotID+"/manifest.json", expired); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 100; n++ {
		if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"snapshots/"+expired.SnapshotID+fmt.Sprintf("/entries/%02d.json", n), true); err != nil {
			t.Fatal(err)
		}
	}
	store := &inventoryMaintenanceStore{Store: f.service.store, delay: 25 * time.Millisecond}
	f.service.store = store
	f.service.cfg.Inventory = f.service.inventoryConfig()
	f.service.cfg.Inventory.BatchSize = 1000
	f.service.cfg.Inventory.BatchTimeout = 20 * time.Second
	state := inventoryCheckpoint{}
	f.service.maintainInventory(ctx, &state)
	if store.timeouts != 0 || inventoryEventCount(t, f.service, "inventory_maintenance_failed") != 0 || inventoryEventCount(t, f.service, "inventory_maintenance_budget_yield") != 1 {
		t.Fatalf("normal budget yield became a failure: timeouts=%d", store.timeouts)
	}
	for _, key := range []string{"retention.json", "work-retention.json"} {
		if _, err := store.Store.Head(ctx, inventory.Prefix+key); err != nil {
			t.Fatalf("cleanup made no progress for %s: %v", key, err)
		}
	}
}

func TestInventoryMaintenanceStorageFailuresRemainVisible(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.store = &inventoryMaintenanceStore{Store: f.service.store, fail: true}
	f.service.maintainInventory(context.Background(), &inventoryCheckpoint{})
	if inventoryEventCount(t, f.service, "inventory_maintenance_failed") != 1 || inventoryEventCount(t, f.service, "inventory_maintenance_budget_yield") != 0 {
		t.Fatal("real storage timeout was classified as a normal maintenance yield")
	}
}
