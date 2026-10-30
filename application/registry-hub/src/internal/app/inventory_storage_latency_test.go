package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"sync/atomic"
	"testing"
	"time"
)

type inventoryLatencyStore struct {
	storage.Store
	delay time.Duration
	calls atomic.Uint64
}

func (s *inventoryLatencyStore) wait(ctx context.Context) error {
	s.calls.Add(1)
	if s.delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *inventoryLatencyStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if err := s.wait(ctx); err != nil {
		return storage.Object{}, err
	}
	return s.Store.Get(ctx, key)
}

func (s *inventoryLatencyStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	if err := s.wait(ctx); err != nil {
		return storage.Entry{}, err
	}
	return s.Store.Head(ctx, key)
}

func (s *inventoryLatencyStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if err := s.wait(ctx); err != nil {
		return storage.Entry{}, err
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *inventoryLatencyStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if err := s.wait(ctx); err != nil {
		return nil, "", err
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func TestInventoryRefreshWithAllStorageReadsDelayed(t *testing.T) {
	local, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	store := &inventoryLatencyStore{Store: local, delay: time.Millisecond}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	roots := 4
	if os.Getenv("REGISTRY_HUB_TEST_INVENTORY_SCALE") == "1" {
		roots = 20
	}
	verifyInventoryStorageLatency(t, f, store, roots)
}

func TestInventoryRefreshAgainstS3(t *testing.T) {
	remote := newDedicatedS3AcceptanceStore(t, "REGISTRY_HUB_TEST_INVENTORY_S3_CONFIG")
	store := &inventoryLatencyStore{Store: remote}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	verifyInventoryStorageLatency(t, f, store, 20)
}

func verifyInventoryStorageLatency(t *testing.T, f *stagedAcceptanceFixture, store *inventoryLatencyStore, roots int) {
	t.Helper()
	enableInventory(f)
	var manifest artifact.Descriptor
	for n := range roots {
		manifest, _, _ = publishAnchoredTestPlatform(f, fmt.Sprintf("storage-latency-%d", n))
	}
	drainBackgroundWork(t, f.service, "inventory", "execution")
	t.Logf("prepared %d completed roots", roots)
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[]}`))
			return
		}
		f.serveHTTP(w, r)
	}))
	ctx := context.Background()
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "storage-latency", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{Fence: leader.FencingToken}
	targets := []time.Duration{time.Minute, 30 * time.Second}
	if roots == 20 {
		targets = []time.Duration{3 * time.Minute, 2 * time.Minute}
	}
	for round, target := range targets {
		started, calls := time.Now(), store.calls.Load()
		phases := map[string]time.Duration{}
		state.NextScanAt = time.Time{}
		for step := 0; ; step++ {
			if step%100 == 0 {
				_, err := f.service.inventoryBinding(ctx, inventoryIdentity("ghcr.io", "team/image", "changing"), manifest.Digest, "upstream", 0, time.Now().UTC(), true)
				if err != nil {
					t.Fatal(err)
				}
			}
			phase := fmt.Sprintf("source/%d", state.Scan.Phase)
			if state.Build != nil {
				phase = fmt.Sprintf("build/%d", state.BuildPhase)
			}
			if state.ImageBuild != nil {
				phase = fmt.Sprintf("image/%d", state.ImageBuild.Phase)
			}
			unit := time.Now()
			if err := f.service.advanceInventory(ctx, leader, &state); err != nil {
				t.Fatal(err)
			}
			state, _, err = inventory.Read[inventoryCheckpoint](ctx, store, inventory.Prefix+"checkpoint.json")
			if err != nil {
				t.Fatal(err)
			}
			phases[phase] += time.Since(unit)
			if state.Build == nil && state.LastCompleteAt.After(started) {
				break
			}
			if time.Since(started) > target {
				t.Fatalf("round %d exceeded %s: source=%d build=%d calls=%d phases=%v", round, target, state.Scan.Phase, state.BuildPhase, store.calls.Load()-calls, phases)
			}
		}
		snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
		if err != nil || snapshot.Coverage != "complete" || snapshot.UniqueManifests != int64(roots) {
			t.Fatalf("snapshot=%+v error=%v", snapshot, err)
		}
		t.Logf("roots=%d round=%d elapsed=%s calls=%d refreshSeconds=%.3f phases=%v", roots, round, time.Since(started), store.calls.Load()-calls, state.LastRefreshSeconds, phases)
	}
}
