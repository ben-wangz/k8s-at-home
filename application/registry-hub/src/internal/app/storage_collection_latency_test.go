package app

import (
	"context"
	"fmt"
	"os"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

type storageCollectionLatencyStore struct {
	*inventoryLatencyStore
	lists     int
	listDelay time.Duration
}

func (s *storageCollectionLatencyStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.lists++
	timer := time.NewTimer(s.listDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, "", ctx.Err()
	case <-timer.C:
	}
	return s.inventoryLatencyStore.List(ctx, prefix, token, limit)
}

func TestStorageCollectionLatency(t *testing.T) {
	if os.Getenv("REGISTRY_HUB_TEST_STORAGE_SCALE") != "1" {
		t.Skip("set REGISTRY_HUB_TEST_STORAGE_SCALE=1 for the 1,000-object-per-domain latency gate")
	}
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	registry, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	verifyStorageCollectionLatency(t, f, registry, 5*time.Millisecond, 45*time.Millisecond)
}

func TestStorageCollectionLatencyAgainstS3(t *testing.T) {
	stateStore := newDedicatedS3AcceptanceStore(t, "REGISTRY_HUB_TEST_STORAGE_COLLECTION_S3_CONFIG")
	registryStore := newDedicatedS3AcceptanceStore(t, "REGISTRY_HUB_TEST_STORAGE_COLLECTION_S3_CONFIG")
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, stateStore)
	enableInventory(f)
	verifyStorageCollectionLatency(t, f, registryStore, 0, 0)
}

func verifyStorageCollectionLatency(t *testing.T, f *stagedAcceptanceFixture, registry storage.Store, delay, listDelay time.Duration) {
	t.Helper()
	ctx := context.Background()
	for n := range 1000 {
		prefix := []string{"cache/fixture/", "tasks/state/", "background/v1/jobs/test/", "diagnostics/v1/events/"}[n%4]
		body := `{"repository":"fixture","authority":"example.org"}`
		if _, err := f.service.store.Put(ctx, fmt.Sprintf("%s%08d.json", prefix, n), strings.NewReader(body), int64(len(body)), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Put(ctx, fmt.Sprintf("docker/registry/v2/blobs/sha256/ab/%064x/data", n), strings.NewReader("fixture"), 7, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	stateStore := &storageCollectionLatencyStore{inventoryLatencyStore: &inventoryLatencyStore{Store: f.service.store, delay: delay}, listDelay: listDelay}
	registryStore := &storageCollectionLatencyStore{inventoryLatencyStore: &inventoryLatencyStore{Store: registry, delay: delay}, listDelay: listDelay}
	f.service.store, f.service.inventory.Store = stateStore, stateStore
	rounds := 3
	if os.Getenv("REGISTRY_HUB_TEST_STORAGE_BASELINE") == "1" {
		rounds = 1
	}
	for round := range rounds {
		state := newStorageCollection()
		started, calls, lists := time.Now(), stateStore.calls.Load()+registryStore.calls.Load(), stateStore.lists+registryStore.lists
		phases := map[string]time.Duration{}
		for steps := 0; state.NextScanAt.IsZero(); steps++ {
			phase, unit := storageCollectionPhase(state), time.Now()
			if err := f.service.advanceStorageCollection(ctx, &state, inspectionReadOnlyStore{registryStore}, nil); err != nil {
				t.Fatal(err)
			}
			if err := inventory.Update(ctx, stateStore, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil }); err != nil {
				t.Fatal(err)
			}
			loaded, _, err := inventory.Read[storageCollection](ctx, stateStore, storageInventoryPrefix+"checkpoint.json")
			if err != nil {
				t.Fatal(err)
			}
			state = loaded
			phases[phase] += time.Since(unit)
			if time.Since(started) > 10*time.Minute {
				t.Fatalf("collection stalled: phase=%s steps=%d", phase, steps)
			}
		}
		snapshot, err := f.service.storageSnapshot(ctx, "")
		if err != nil || snapshot.Domains["state"].State != "observed" || snapshot.Domains["registry"].State != "observed" || *snapshot.Domains["registry"].Objects != 1000 {
			t.Fatalf("snapshot=%+v err=%v", snapshot, err)
		}
		t.Logf("round=%d elapsed=%s calls=%d lists=%d phases=%v", round, time.Since(started), stateStore.calls.Load()+registryStore.calls.Load()-calls, stateStore.lists+registryStore.lists-lists, phases)
		if rounds > 1 && time.Since(started) > 180*time.Second {
			t.Fatal("physical collection exceeded unchanged 180-second gate")
		}
	}
}
