package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
)

func TestInventoryDeltaFanoutReplaysPartialInvalidationWithoutAdvancingCursor(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	p := inventoryDeltaProgress{Fence: 20}
	for key := range fragment.Dependencies.Objects {
		p.Changes = []inventoryChange{{Key: key, Revision: "captured-change", SourceVersion: "changed"}}
		break
	}
	keys := []string{}
	for index := range 4 {
		fragment.Image.ID = fmt.Sprintf("fanout-%d", index)
		key := inventory.Prefix + "fragments/image/" + fragment.Image.ID + ".json"
		if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryFragment) error { *value = fragment; return nil }); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	p.Fanout = inventory.Cursor{Entries: []storage.Entry{{Key: keys[0]}, {Key: keys[1]}, {Key: keys[2]}, {Key: keys[3]}}}
	fault := &inventoryJournalFailureStore{Store: f.service.store, key: keys[1], failRead: true}
	f.service.store = fault
	if err := f.service.invalidateInventoryFragments(ctx, &p); !errors.Is(err, storage.ErrTimeout) || p.Fanout.Offset != 0 {
		t.Fatalf("failed fanout advanced checkpoint: offset=%d error=%v", p.Fanout.Offset, err)
	}
	dirty, _, err := inventory.Read[inventoryDirtyEntity](ctx, fault, inventoryDirtyKey("fanout-0"))
	if err != nil || dirty.Revision == "" || dirty.Fence != 20 {
		t.Fatalf("fixture did not commit partial invalidation: %+v error=%v", dirty, err)
	}
	checkpoint, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p = inventoryDeltaProgress{}
	if err := json.Unmarshal(checkpoint, &p); err != nil {
		t.Fatal(err)
	}
	fault.failRead = false
	if err := f.service.invalidateInventoryFragments(ctx, &p); err != nil || !p.Fanout.Done {
		t.Fatalf("fanout replay did not finish the captured page: cursor=%+v error=%v", p.Fanout, err)
	}
	for index := range 4 {
		current, _, err := inventory.Read[inventoryDirtyEntity](ctx, fault, inventoryDirtyKey(fmt.Sprintf("fanout-%d", index)))
		if err != nil || current.Revision == "" || current.Fence != 20 || index == 0 && current.Revision != dirty.Revision {
			t.Fatalf("fanout replay lost or changed invalidation %d: %+v error=%v", index, current, err)
		}
	}
}
