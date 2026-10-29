package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func projectionRequestFixture(t *testing.T, count int) (*Service, *projectionRequestStore) {
	t.Helper()
	children := make([]artifact.Descriptor, count)
	for n := range children {
		children[n] = artifact.Descriptor{MediaType: ociManifestMediaType, Digest: artifact.CalculateSHA256([]byte(fmt.Sprint(n))), Size: 1}
	}
	data, err := json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	if err != nil {
		t.Fatal(err)
	}
	remote := &scriptedUpstream{results: []upstreamResult{{status: http.StatusOK, contentType: ociIndexMediaType, body: string(data)}}}
	s, _, _ := newDiscoveryTestService(t, remote)
	store := &projectionRequestStore{Store: s.store}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	s.store, s.tasks, s.records = store, manager, tasks.NewRecords(store)
	s.cache = artifact.NewCache(store)
	s.cache.EnableInventoryTracking()
	s.inventory = &inventory.Repository{Store: store}
	s.initializeInventory(context.Background())
	s.cfg.Limits.MaxDescriptors = 1000
	s.cache.SetProjectionRetention(s.retainUnprojectedManifest)
	store.operations = 0
	store.delay = time.Millisecond
	return s, store
}

func TestManifestRequestWorkDoesNotGrowWithDescriptorCount(t *testing.T) {
	baseline := 0
	for _, count := range []int{1, 128} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, store := projectionRequestFixture(t, count)
			_, _, routed := newDiscoveryTestService(t, &scriptedUpstream{})
			started := time.Now()
			response := serveDiscoveryRequest(t, s, routed, "*/*")
			calls := store.operations
			if response.Code != http.StatusOK || store.associations != 0 {
				t.Fatalf("response=%d synchronous associations=%d", response.Code, store.associations)
			}
			if baseline == 0 {
				baseline = calls
			} else if calls > baseline+2 {
				t.Fatalf("request calls grew from %d to %d", baseline, calls)
			}
			t.Logf("descriptors=%d request storage operations=%d elapsed=%s", count, calls, time.Since(started))
			drainBackgroundWork(t, s, "inventory")
			task := discoveryTask(t, s.tasks)
			content, err := s.inventory.Content(context.Background(), inventoryIdentity(task.Authority, task.Repository, task.Reference), task.RootDigest)
			if err != nil || !content.ClosureKnown || len(content.Children) != count {
				t.Fatalf("projected children=%d err=%v", len(content.Children), err)
			}
			before := store.operations
			if _, err := s.inventoryContent(context.Background(), content.Identity, content.Digest, content.MediaType, response.Body.Bytes()); err != nil {
				t.Fatal(err)
			}
			t.Logf("synchronous projection baseline operations=%d", store.operations-before)
			if count == 128 && store.operations-before <= calls {
				t.Fatal("fixture did not exercise descriptor-dependent baseline")
			}
		})
	}
}

func TestManifestSourceSurvivesFailedEnqueueAndCacheExpiry(t *testing.T) {
	s, store := projectionRequestFixture(t, 4)
	store.rejectQueue = true
	_, _, routed := newDiscoveryTestService(t, &scriptedUpstream{})
	response := serveDiscoveryRequest(t, s, routed, "*/*")
	if response.Code != http.StatusOK {
		t.Fatalf("enqueue failure failed committed request: %d", response.Code)
	}
	task := discoveryTask(t, s.tasks)
	if task.ManifestObservedAt.IsZero() {
		t.Fatal("source observation was not persisted")
	}
	if removed, err := s.cache.Cleanup(context.Background(), time.Now().Add(48*time.Hour), 100); err != nil || removed != 0 {
		t.Fatalf("sole recovery source removed=%d err=%v", removed, err)
	}
	store.rejectQueue = false
	s.inventoryDirty.Store(false)
	if err := s.reconcileInventoryTask(context.Background(), "tasks/state/"+task.ID+".json"); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, s, "inventory")
	binding, _, err := inventory.Read[inventory.Binding](context.Background(), store, inventory.Prefix+"bindings/"+inventoryIdentity(task.Authority, task.Repository, task.Reference).ID()+".json")
	if err != nil || binding.Digest != task.RootDigest || !binding.ConfirmedAt.Equal(task.ManifestObservedAt) {
		t.Fatalf("recovered binding=%+v err=%v", binding, err)
	}
}

type projectionRequestStore struct {
	storage.Store
	operations, associations int
	rejectQueue              bool
	delay                    time.Duration
}

func (s *projectionRequestStore) Get(ctx context.Context, key string) (storage.Object, error) {
	s.operations++
	time.Sleep(s.delay)
	return s.Store.Get(ctx, key)
}
func (s *projectionRequestStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	s.operations++
	time.Sleep(s.delay)
	if strings.HasPrefix(key, inventory.Prefix+"object-links/") {
		s.associations++
	}
	if s.rejectQueue && strings.HasPrefix(key, "background/v1/jobs/") {
		return storage.Entry{}, storage.ErrUnavailable
	}
	return s.Store.Put(ctx, key, body, size, opts)
}

func (s *projectionRequestStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	s.operations++
	return s.Store.Head(ctx, key)
}
func (s *projectionRequestStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.operations++
	return s.Store.List(ctx, prefix, token, limit)
}
func (s *projectionRequestStore) Delete(ctx context.Context, key string) error {
	s.operations++
	return s.Store.Delete(ctx, key)
}
func (s *projectionRequestStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.operations++
	return s.Store.DeleteIfMatch(ctx, key, version)
}
