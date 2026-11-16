package app

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

type inventoryExpiryStore struct {
	storage.Store
	claimed bool
	expired bool
	initial string
	ids     map[string]bool
	cancel  context.CancelFunc
}

func (s *inventoryExpiryStore) Get(ctx context.Context, key string) (storage.Object, error) {
	object, err := s.Store.Get(ctx, key)
	if err == nil && key == inventory.Prefix+"current.json" && s.claimed && !s.expired {
		s.expired = true
		err = inventory.Update(ctx, s.Store, key, func(value *inventory.Snapshot) error { value.ExpiresAt = time.Now().Add(-time.Second); return nil })
		if err != nil {
			_ = object.Body.Close()
			return storage.Object{}, err
		}
	}
	return object, err
}

func (s *inventoryExpiryStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, opts)
	if err == nil && key == inventory.Prefix+"checkpoint.json" {
		s.claimed = true
	}
	if err == nil && key == inventory.Prefix+"current.json" {
		current, _, readErr := inventory.Read[inventory.Snapshot](ctx, s.Store, key)
		if readErr != nil {
			return entry, readErr
		}
		if current.SnapshotID != s.initial {
			s.ids[current.SnapshotID] = true
			if len(s.ids) >= 2 {
				s.cancel()
			}
		}
	}
	return entry, err
}

func TestInventoryLeaderRenewsPlaceholderWhileWorking(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.Inventory = f.service.inventoryConfig()
	f.service.cfg.Inventory.SnapshotRetention = 2 * time.Second
	f.service.cfg.Inventory.BatchSize = 1
	f.service.cfg.Inventory.BatchTimeout = time.Second
	initial := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("initial"), BuildStartedAt: time.Now().UTC(), ObservedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour), Coverage: "partial", Building: true, Reasons: []string{"index_backfilling"}}
	seedInventoryCurrent(t, f.service, initial)
	seedInventoryHistoricalGraphs(t, f)
	// Two observed publications cancel the run; this deadline is a failure watchdog.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "runtime", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(context.Background(), leader)
	store := &inventoryExpiryStore{Store: f.service.store, initial: initial.SnapshotID, ids: map[string]bool{}, cancel: cancel}
	f.service.store, f.service.inventory.Store = store, store
	if err := f.service.runInventoryLeader(ctx, leader); !errors.Is(err, context.Canceled) {
		t.Fatalf("leader failed before repeated runtime renewal: err=%v parent=%v expired=%t publications=%d", err, ctx.Err(), store.expired, len(store.ids))
	}
	if !store.expired || len(store.ids) != 2 {
		t.Fatalf("runtime renewal did not run: expired=%v publications=%d", store.expired, len(store.ids))
	}
	state, _, err := inventory.Read[inventoryCheckpoint](context.Background(), store.Store, inventory.Prefix+"checkpoint.json")
	if err != nil || state.Steps < 2 {
		t.Fatalf("unexpected builder progress: %+v err=%v", state, err)
	}
}

func TestInventoryPlaceholderRenewsDuringEstablishedLeaderIdle(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.Inventory = f.service.inventoryConfig()
	f.service.cfg.Inventory.SnapshotRetention = 2 * time.Second
	initial := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("idle"), BuildStartedAt: time.Now().UTC(), ObservedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(-time.Second), Coverage: "partial", Building: true, Reasons: []string{"index_backfilling"}}
	seedInventoryCurrent(t, f.service, initial)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "idle", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(context.Background(), leader)
	store := &inventoryExpiryStore{Store: f.service.store, initial: initial.SnapshotID, ids: map[string]bool{}, cancel: cancel}
	f.service.store, f.service.inventory.Store = store, store
	state := inventoryCheckpoint{Steps: 31, NextScanAt: time.Now().Add(time.Hour)}
	availability := inventoryAvailability{}
	for ctx.Err() == nil {
		if !f.service.inventoryIdle(&state) {
			t.Fatal("fixture stopped representing an established idle leader")
		}
		if err := f.service.checkInventoryAvailability(ctx, leader, &state, &availability); err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !errors.Is(ctx.Err(), context.Canceled) || len(store.ids) != 2 || state.Steps != 31 {
		t.Fatalf("idle renewal changed builder progress or failed: publications=%d state=%+v err=%v", len(store.ids), state, ctx.Err())
	}
}
