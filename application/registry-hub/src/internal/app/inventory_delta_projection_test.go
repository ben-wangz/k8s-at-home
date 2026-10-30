package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
)

func TestInventoryDeltaReusesGraphProjectionAcknowledgedByBufferedTask(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.Background()
	root := ""
	for _, task := range fragment.Image.Solidification.Tasks {
		if task.Kind == "root" {
			root = task.ID
		}
	}
	if root == "" {
		t.Fatal("fixture omitted its root task")
	}
	keys := []string{"tasks/state/" + root + ".json", "solidification/graphs/" + root + "/"}
	p := inventoryDeltaProgress{}
	for _, key := range keys {
		version := ""
		if key == keys[0] {
			entry, err := f.service.store.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			version = entry.Version
		}
		if err := f.service.recordInventoryChange(ctx, key, version); err != nil {
			t.Fatal(err)
		}
		change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(key))
		if err != nil {
			t.Fatal(err)
		}
		p.Changes = append(p.Changes, change)
	}
	spy := &inventoryBackfillStore{Store: f.service.store}
	f.service.store = spy
	for step := 0; step < 10000; step++ {
		done, err := f.service.projectInventoryChange(ctx, &p)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		p = inventoryDeltaProgress{}
		if err := json.Unmarshal(data, &p); err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
		if step == 9999 {
			t.Fatal("task projection did not converge")
		}
	}
	if spy.edgeReads == 0 {
		t.Fatal("pending graph marker did not force projection")
	}
	if _, err := spy.Head(ctx, inventoryChangeKey(keys[1])); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("task projection did not retire its captured graph marker: %v", err)
	}
	p.Offset, spy.edgeReads = 1, 0
	if _, err := f.service.projectInventoryChange(ctx, &p); err != nil || p.Source == nil || !p.Source.Complete {
		t.Fatalf("already projected buffered graph was rebuilt: projection=%+v error=%v", p.Source, err)
	}
	if done, err := f.service.projectInventoryChange(ctx, &p); err != nil || !done || spy.edgeReads != 0 {
		t.Fatalf("buffered graph reread edges: done=%v reads=%d error=%v", done, spy.edgeReads, err)
	}
	if err := f.service.recordInventoryChange(ctx, keys[1], ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.projectInventoryChange(ctx, &p); err != nil || p.Source == nil || p.Source.Complete {
		t.Fatalf("new graph revision reused the old projection: projection=%+v error=%v", p.Source, err)
	}
}
