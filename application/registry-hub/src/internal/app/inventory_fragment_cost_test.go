package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

type fragmentCostStore struct {
	storage.Store
	calls int
}

func (s *fragmentCostStore) Get(ctx context.Context, key string) (storage.Object, error) {
	s.calls++
	return s.Store.Get(ctx, key)
}

func (s *fragmentCostStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	s.calls++
	return s.Store.Head(ctx, key)
}

func (s *fragmentCostStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.calls++
	return storage.Scan(ctx, s.Store, prefix, token, limit)
}

func TestInventoryFastFragmentControlCallsDoNotScaleWithDependencies(t *testing.T) {
	var baseline int
	for _, count := range []int{1, 100, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			ctx := context.Background()
			image := inventory.Image{Identity: inventory.Identity{Authority: "example.org", Repository: "test/image"}, ContentID: "content"}
			image.ID = image.Identity.ID()
			image.Evidence.CheckedAt = time.Now().UTC()
			fragment := inventoryFragment{Image: image, CapturedAt: image.Evidence.CheckedAt, Closure: inventoryClosureProgress{Complete: true}, Epoch: f.service.gate.State().Epoch, ReuseVersion: 1, Dependencies: inventoryDependencies{Objects: map[string]string{}}}
			for n := range count {
				fragment.Dependencies.Objects[fmt.Sprintf("tasks/state/%08d.json", n)] = "captured-version"
			}
			key := inventoryFragmentKey(image, "image")
			if err := inventory.Update(ctx, f.service.store, key, func(value *inventoryFragment) error { *value = fragment; return nil }); err != nil {
				t.Fatal(err)
			}
			spy := &fragmentCostStore{Store: f.service.store}
			f.service.store = spy
			p := inventoryImageProgress{Image: image, Epoch: fragment.Epoch}
			if err := f.service.loadInventoryFragment(ctx, &p); err != nil || p.Reuse == nil {
				t.Fatalf("load fragment: %v", err)
			}
			if err := f.service.advanceInventoryFragment(ctx, &p); err != nil {
				t.Fatal(err)
			}
			if p.Reuse != nil || p.Phase != 6 || !p.Image.Evidence.CheckedAt.Equal(fragment.CapturedAt) {
				t.Fatal("fast reuse did not finish or renewed old evidence")
			}
			if count == 1 {
				baseline = spy.calls
			}
			if spy.calls != baseline || spy.calls > 6 {
				t.Fatalf("dependency count %d used %d control calls, baseline %d", count, spy.calls, baseline)
			}
		})
	}
}

func TestInventoryFullReconciliationDisablesFastFragmentValidation(t *testing.T) {
	f, fragment := inventoryFragmentFixture(t)
	ctx := context.WithValue(context.Background(), inventoryFragmentValidationKey{}, true)
	p := inventoryImageProgress{Image: fragment.Image, Epoch: fragment.Epoch, DirtyRevision: fragment.DirtyRevision}
	if err := f.service.loadInventoryFragment(ctx, &p); err != nil || p.Reuse == nil || p.Reuse.Fast {
		t.Fatalf("full reconciliation allowed fast reuse: %v", err)
	}
	// A missing marker cannot conceal changed physical evidence from full validation.
	for key := range fragment.Dependencies.Objects {
		if err := f.service.store.(*observedStore).Store.Delete(ctx, key); err != nil {
			continue
		}
		for step := 0; p.Reuse != nil && step < 10000; step++ {
			if err := f.service.advanceInventoryFragment(ctx, &p); err != nil {
				t.Fatal(err)
			}
		}
		if p.Phase == 6 {
			t.Fatal("full validation reused changed source without its marker")
		}
		return
	}
	t.Fatal("fixture had no physical dependency")
}
