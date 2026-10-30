package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"sync"
	"testing"
	"time"
)

type stagedAcceptanceObject struct {
	body      []byte
	mediaType string
}

type stagedAcceptanceFixture struct {
	t             *testing.T
	service       *Service
	processor     *solidifier
	mu            sync.Mutex
	objects       map[string]stagedAcceptanceObject
	uploads       map[string]int
	requests      int
	beforeRequest func(*http.Request)
}

func newStagedAcceptanceFixture(t *testing.T, maxBytes int64) *stagedAcceptanceFixture {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return newStagedAcceptanceFixtureWithStore(t, maxBytes, store)
}

func newStagedAcceptanceFixtureWithStore(t *testing.T, maxBytes int64, store storage.Store) *stagedAcceptanceFixture {
	t.Helper()
	store = newObservedStore(store, newMetrics(), nil)
	ctx := context.Background()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	cfg := config.Config{
		Limits: config.Limits{RequestTimeout: 30 * time.Second, BlobTransferTimeout: 30 * time.Second,
			ManifestBytes: 1 << 20, ObjectBytes: 1 << 20, MaxDescriptors: 100,
			DescriptorDataBytes: 1 << 20, ClosureDepth: 10, ClosureObjects: 100, ClosureBytes: 1 << 20},
		Solidification: config.Solidification{MaxProtectionSessions: 10,
			MaxProtectedCacheBytes: 1 << 20, MaxUnanchoredDistributionBytes: maxBytes},
	}
	cache := artifact.NewCache(store)
	f := &stagedAcceptanceFixture{t: t, objects: make(map[string]stagedAcceptanceObject), uploads: make(map[string]int)}
	registry := newLocalRegistry(t, http.HandlerFunc(f.serveHTTP))
	f.service = &Service{cfg: cfg, store: store, cache: cache, registry: registry, gate: gate,
		records: tasks.NewRecords(store), tasks: manager, metrics: newMetrics(), solidificationSlots: make(chan struct{}, 1)}
	f.processor = &solidifier{service: f.service, cfg: cfg, cache: cache, store: store,
		registry: registry, gate: gate, tasks: manager}
	store.(*observedStore).inventoryChange = func(ctx context.Context, key, version string) error {
		if f.service.inventory == nil {
			return nil
		}
		return f.service.recordInventoryChange(ctx, key, version)
	}
	store.(*observedStore).inventoryNotify = func() { f.service.inventoryDirty.Store(true) }
	return f
}

func (f *stagedAcceptanceFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if f.beforeRequest != nil {
		f.beforeRequest(r)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if strings.HasSuffix(r.URL.Path, "/blobs/uploads/") && r.Method == http.MethodPost {
		w.Header().Set("Location", r.URL.Path+"acceptance")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if strings.Contains(r.URL.Path, "/blobs/uploads/") && r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		digest := r.URL.Query().Get("digest")
		if err != nil || artifact.CalculateSHA256(body) != digest {
			http.Error(w, "invalid upload", http.StatusBadRequest)
			return
		}
		f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
		f.uploads[digest]++
		w.Header().Set("Docker-Content-Digest", digest)
		w.WriteHeader(http.StatusCreated)
		return
	}
	index := strings.LastIndex(r.URL.Path, "/manifests/")
	manifest := index >= 0
	if !manifest {
		index = strings.LastIndex(r.URL.Path, "/blobs/")
	}
	if index < 0 {
		http.NotFound(w, r)
		return
	}
	reference := r.URL.Path[index+len("/blobs/"):]
	if manifest {
		reference = r.URL.Path[index+len("/manifests/"):]
	}
	if manifest && r.Method == http.MethodPut {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid manifest", http.StatusBadRequest)
			return
		}
		f.objects[reference] = stagedAcceptanceObject{body: body, mediaType: r.Header.Get("Content-Type")}
		w.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(body))
		w.Header().Set("Location", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		return
	}
	object, ok := f.objects[reference]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
		}
		return
	}
	w.Header().Set("Content-Type", object.mediaType)
	w.Header().Set("Content-Length", fmt.Sprint(len(object.body)))
	w.Header().Set("Docker-Content-Digest", artifact.CalculateSHA256(object.body))
	if r.Method != http.MethodHead {
		_, _ = w.Write(object.body)
	}
}

func (f *stagedAcceptanceFixture) cacheObject(body []byte, mediaType string) artifact.Descriptor {
	f.t.Helper()
	descriptor := artifact.Descriptor{Digest: artifact.CalculateSHA256(body), Size: int64(len(body)), MediaType: mediaType}
	if err := f.service.cache.Put(context.Background(), "ghcr.io", "team/image", descriptor.Digest,
		mediaType, bytes.NewReader(body), descriptor.Size); err != nil {
		f.t.Fatal(err)
	}
	return descriptor
}

func (f *stagedAcceptanceFixture) cacheManifest(manifest artifact.Manifest) artifact.Descriptor {
	f.t.Helper()
	body, err := json.Marshal(manifest)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.cacheObject(body, manifest.MediaType)
}

func (f *stagedAcceptanceFixture) root(reference string, descriptor artifact.Descriptor) tasks.Task {
	f.t.Helper()
	task, _, err := f.service.tasks.EnqueueResolved(context.Background(), "ghcr.io", "team/image",
		reference, descriptor.Digest, descriptor.MediaType, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}
