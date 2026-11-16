package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

type inventoryMarkerFailureStore struct{ storage.Store }

func (s inventoryMarkerFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	if strings.HasPrefix(key, inventory.Prefix+"changes/") {
		return storage.Entry{}, storage.ErrTimeout
	}
	return s.Store.Put(ctx, key, body, size, opts)
}

func TestInventoryBuilderWritesDoNotCreateChangeMarkers(t *testing.T) {
	for _, reason := range []string{"inventory_snapshot_build", "inventory_full_reconcile", "inventory_refresh", "inventory_maintenance"} {
		t.Run(reason, func(t *testing.T) {
			ctx := withStorageReason(context.Background(), reason)
			var notified bool
			var changes []string
			observed := &observedStore{
				inventoryNotify: func() { notified = true },
				inventoryChange: func(_ context.Context, key, _ string) error {
					changes = append(changes, key)
					return nil
				},
			}
			for _, key := range []string{
				inventory.Prefix + "contents/content.json",
				inventory.Prefix + "bindings/image.json",
				inventory.Prefix + "proofs/content.json",
				inventory.Prefix + "projection-revisions/image.json",
			} {
				observed.inventoryChanged(ctx, key, "version", nil)
			}
			if notified || len(changes) != 0 {
				t.Fatalf("inventory builder outputs created a source change: notified=%t changes=%v", notified, changes)
			}

			const sourceKey = "tasks/state/task.json"
			observed.inventoryChanged(ctx, sourceKey, "source-version", nil)
			if !notified || len(changes) != 1 || changes[0] != sourceKey {
				t.Fatalf("builder context suppressed a real source change: notified=%t changes=%v", notified, changes)
			}
			observed.inventoryChanged(ctx, "tasks/dispatch/v1/repair.json", "maintenance-version", nil)
			if len(changes) != 1 {
				t.Fatalf("unrelated task dispatch state created an inventory change: %v", changes)
			}
		})
	}
}

func TestInventoryProjectionRevisionChangesRemainJournaled(t *testing.T) {
	var notified bool
	var changes []string
	observed := &observedStore{
		inventoryNotify: func() { notified = true },
		inventoryChange: func(_ context.Context, key, _ string) error {
			changes = append(changes, key)
			return nil
		},
	}
	const key = inventory.Prefix + "projection-revisions/image.json"
	observed.inventoryChanged(context.Background(), key, "version", nil)
	if !notified || len(changes) != 1 || changes[0] != key {
		t.Fatalf("asynchronous projection revision was not journaled: notified=%t changes=%v", notified, changes)
	}
}

func TestInventoryMarkerFailureRetainsSourceOutcomeAndRejectsStaleFragment(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	observed := f.service.store.(*observedStore)
	original := observed.Store
	observed.Store = inventoryMarkerFailureStore{Store: original}
	key := inventory.Prefix + "proofs/" + fragment.Image.ContentID + ".json"
	if err := inventory.Update(context.Background(), observed, key, func(value *inventoryProof) error { value.ObservedAt = time.Now().UTC(); return nil }); err != nil {
		t.Fatalf("optional marker failure changed committed source outcome: %v", err)
	}
	observed.Store = original
	if p := resumeInventoryFragment(t, f.service, fragment); p.Phase != 0 {
		t.Fatal("failed change marker allowed reuse of an old source version")
	}
	state := inventoryCheckpoint{Scan: inventoryScan{Complete: true}, NextReconcileAt: time.Now().Add(-time.Second)}
	if _, err := f.service.advanceInventorySources(context.Background(), &state); err != nil || !state.NextReconcileAt.IsZero() || !state.Scan.Complete {
		t.Fatalf("missed marker reconciliation was not checkpointed before advancing: state=%+v error=%v", state, err)
	}
	if _, err := f.service.advanceInventorySources(context.Background(), &state); err != nil || state.Delta != nil || state.Scan.Complete {
		t.Fatalf("periodic reconciliation did not recover missed markers: state=%+v error=%v", state, err)
	}
}

func TestInventoryDeltaReplaysFanoutAndPreservesNewerChange(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	key := inventory.Prefix + "proofs/" + fragment.Image.ContentID + ".json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryProof) error { value.ObservedAt = time.Now().UTC(); return nil }); err != nil {
		t.Fatal(err)
	}
	change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
	if err != nil {
		t.Fatal(err)
	}
	p := inventoryDeltaProgress{Fence: fragment.Fence, Changes: []inventoryChange{change}, Phase: 2, Cursor: inventory.Cursor{Done: true}}
	newer := ""
	for step := 0; step < 100; step++ {
		if p.Phase == 3 && newer == "" {
			if err := f.service.recordInventoryChange(ctx, key, "newer-version"); err != nil {
				t.Fatal(err)
			}
			value, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
			if err != nil {
				t.Fatal(err)
			}
			newer = value.Revision
		}
		before, _ := json.Marshal(p)
		done, err := f.service.advanceInventoryDelta(ctx, &p)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if step%2 == 0 {
			// Replay a unit whose side effect committed before its outer checkpoint.
			p = inventoryDeltaProgress{}
			if err := json.Unmarshal(before, &p); err != nil {
				t.Fatal(err)
			}
		} else {
			data, _ := json.Marshal(p)
			p = inventoryDeltaProgress{}
			if err := json.Unmarshal(data, &p); err != nil {
				t.Fatal(err)
			}
		}
		if step == 99 {
			t.Fatal("delta replay failed to converge")
		}
	}
	value, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
	if err != nil || value.Revision != newer || newer == "" {
		t.Fatalf("old fanout acknowledged a newer source: %+v error=%v", value, err)
	}
	dirty, _, err := inventory.Read[inventoryDirtyEntity](ctx, f.service.store, inventoryDirtyKey(fragment.Image.ID))
	if err != nil || dirty.Revision == "" || dirty.Captured == dirty.Revision {
		t.Fatalf("fanout did not retain dirty image work: %+v error=%v", dirty, err)
	}
	if err := f.service.acknowledgeInventoryChange(ctx, value); err != nil {
		t.Fatal(err)
	}
	if _, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("exact source revision was not retired: %v", err)
	}
}

func TestInventoryCaptureAcknowledgesOnlyItsDirtyRevision(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	id := inventory.ID("dirty-revision-test")
	if err := inventory.Update(ctx, f.service.store, inventoryDirtyKey(id), func(value *inventoryDirtyEntity) error { value.Revision = "newer"; return nil }); err != nil {
		t.Fatal(err)
	}
	p := inventoryImageProgress{Image: inventory.Image{ID: id}, DirtyRevision: "older"}
	if err := f.service.acknowledgeInventoryImage(ctx, &p); err != nil {
		t.Fatal(err)
	}
	value, _, err := inventory.Read[inventoryDirtyEntity](ctx, f.service.store, inventoryDirtyKey(id))
	if err != nil || value.Captured != "" {
		t.Fatalf("older capture acknowledged newer dirty work: %+v error=%v", value, err)
	}
	p.DirtyRevision = "newer"
	if err := f.service.acknowledgeInventoryImage(ctx, &p); err != nil {
		t.Fatal(err)
	}
	value, _, err = inventory.Read[inventoryDirtyEntity](ctx, f.service.store, inventoryDirtyKey(id))
	if err != nil || value.Captured != "newer" {
		t.Fatalf("matching capture was not acknowledged: %+v error=%v", value, err)
	}
}
