package app

import (
	"context"
	"errors"
	"io"
	"reflect"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func seedInventoryCurrent(t *testing.T, s *Service, snapshot inventory.Snapshot) {
	t.Helper()
	ctx := context.Background()
	if err := s.inventory.Immutable(ctx, inventory.Prefix+"snapshots/"+snapshot.SnapshotID+"/manifest.json", snapshot); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, s.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error { *value = snapshot; return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryPlaceholderRefreshPreservesBuildAndPublication(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "refresh", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	writer := f.service.inventory.NewWriter(time.Now().Add(-2*time.Hour).UTC(), time.Hour, leader.FencingToken)
	gate, err := f.service.inventoryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer.Snapshot.GCEpoch = gate.Epoch
	state := inventoryCheckpoint{Fence: leader.FencingToken, Build: &writer.Snapshot, Steps: 73, Scan: inventoryScan{Complete: true}}
	before := state
	initial := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("initial"), BuildStartedAt: writer.Snapshot.BuildStartedAt.Add(-time.Nanosecond), ObservedAt: writer.Snapshot.BuildStartedAt.Add(-time.Nanosecond), ExpiresAt: time.Now().Add(-time.Second).UTC(), BuilderFence: leader.FencingToken, Coverage: "partial", Building: true, Reasons: []string{"index_backfilling"}}
	seedInventoryCurrent(t, f.service, initial)
	availability := inventoryAvailability{}
	seen := map[string]bool{initial.SnapshotID: true}
	for cycle := 0; cycle < 3; cycle++ {
		if err := f.service.refreshInventoryPlaceholder(ctx, leader, &state, &availability); err != nil {
			t.Fatal(err)
		}
		current, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
		if err != nil || seen[current.SnapshotID] || !initialInventoryPlaceholder(current) || time.Until(current.ExpiresAt) < 59*time.Minute || !current.ObservedAt.Equal(initial.ObservedAt) {
			t.Fatalf("refresh %d: snapshot=%+v err=%v", cycle, current, err)
		}
		seen[current.SnapshotID] = true
		if !reflect.DeepEqual(before, state) {
			t.Fatal("placeholder refresh changed build progress")
		}
		// Move only the current fixture's deadline into the renewal window.
		if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error { value.ExpiresAt = time.Now().Add(time.Minute); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	old, _, err := inventory.Read[inventory.Snapshot](ctx, f.service.store, inventory.Prefix+"snapshots/"+initial.SnapshotID+"/manifest.json")
	if err != nil || !reflect.DeepEqual(old, initial) {
		t.Fatal("refresh rewrote the immutable old snapshot")
	}
	if inventoryEventCount(t, f.service, "inventory_placeholder_published") != 3 || inventoryEventCount(t, f.service, "inventory_snapshot_published") != 0 {
		t.Fatal("placeholder refresh counted as completed inventory publication")
	}
	for step := 0; state.Build != nil && step < 30; step++ {
		if err := f.service.advanceInventory(ctx, leader, &state); err != nil {
			t.Fatal(err)
		}
	}
	current, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || state.Build != nil || current.SnapshotID != writer.Snapshot.SnapshotID || current.Coverage != "complete" {
		t.Fatalf("real build blocked by placeholder: snapshot=%+v err=%v", current, err)
	}
}

type inventoryPublicationFault struct {
	storage.Store
	fail    bool
	unknown bool
}

func (s *inventoryPublicationFault) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	if key == inventory.Prefix+"current.json" && s.fail {
		s.fail = false
		if !s.unknown {
			return storage.Entry{}, storage.ErrTimeout
		}
		if _, err := s.Store.Put(ctx, key, body, size, opts); err != nil {
			return storage.Entry{}, err
		}
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return s.Store.Put(ctx, key, body, size, opts)
}

func TestInventoryPlaceholderRetryIsIndependentAndIdempotent(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "unknown_commit"}[unknown], func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			ctx := context.Background()
			leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "retry", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer f.service.tasks.Leases().Release(ctx, leader)
			fault := &inventoryPublicationFault{Store: f.service.store, fail: true, unknown: unknown}
			f.service.store, f.service.inventory.Store = fault, fault
			state := inventoryCheckpoint{Steps: 57}
			availability := inventoryAvailability{}
			if err := f.service.checkInventoryAvailability(ctx, leader, &state, &availability); err != nil {
				t.Fatal(err)
			}
			if !unknown {
				if availability.pending == nil || availability.retryDelay != time.Second || state.Steps != 57 {
					t.Fatal("refresh failure did not retain its own retry state")
				}
				id := availability.pending.SnapshotID
				manifest, _, err := inventory.Read[inventory.Snapshot](ctx, fault, inventory.Prefix+"snapshots/"+id+"/manifest.json")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.checkInventoryAvailability(ctx, leader, &state, &availability); err != nil || availability.pending.SnapshotID != id {
					t.Fatal("backoff failed to preserve the pending publication")
				}
				availability.nextCheckAt = time.Time{}
				if err := f.service.checkInventoryAvailability(ctx, leader, &state, &availability); err != nil {
					t.Fatal(err)
				}
				current, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
				if err != nil || current.SnapshotID != id || !current.ExpiresAt.Equal(manifest.ExpiresAt) {
					t.Fatalf("retry changed immutable identity or expiry: %+v err=%v", current, err)
				}
			}
			if _, err := f.service.inventory.Snapshot(ctx, "", time.Minute); err != nil || availability.pending != nil || availability.retryDelay != 0 {
				t.Fatalf("publication did not recover: err=%v", err)
			}
			if err := f.service.tasks.Leases().Release(ctx, leader); err != nil {
				t.Fatal(err)
			}
			availability.nextCheckAt = time.Time{}
			if err := f.service.store.Delete(ctx, inventory.Prefix+"current.json"); err != nil {
				t.Fatal(err)
			}
			if err := f.service.checkInventoryAvailability(ctx, leader, &state, &availability); !errors.Is(err, lease.ErrLost) {
				t.Fatalf("lost lease was treated as refresh backoff: %v", err)
			}
		})
	}
}
