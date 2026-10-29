package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

type inventoryWriteGuard struct {
	storage.Store
	writes        int
	failLifecycle bool
}

func (s *inventoryWriteGuard) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	s.writes++
	if s.failLifecycle && strings.HasPrefix(key, inventory.Prefix+"lifecycle/") {
		return storage.Entry{}, storage.ErrTimeout
	}
	return s.Store.Put(ctx, key, body, size, opts)
}
func (s *inventoryWriteGuard) Delete(ctx context.Context, key string) error {
	s.writes++
	return s.Store.Delete(ctx, key)
}
func (s *inventoryWriteGuard) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.writes++
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestInventoryAdminReadsAreAuthenticatedAndHaveNoBusinessSideEffects(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	configDescriptor := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configDescriptor, Layers: []artifact.Descriptor{}})
	f.root("latest", manifest)
	snapshot := refreshTestInventory(t, f.service)
	id := inventoryIdentity("ghcr.io", "team/image", "latest").ID()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "operator", Password: "secret", Permissions: []string{auth.TaskAdmin}}, {Username: "gc", Password: "secret", Permissions: []string{auth.GCAdmin}}}}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store = guard
	f.service.inventory.Store = guard
	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	handler := f.service.AdminHandler()
	paths := []string{"/api/v1/images", "/api/v1/images/summary", "/api/v1/images/status", "/api/v1/images/" + id, "/api/v1/images/" + id + "/objects", "/api/v1/images/" + id + "/platforms", "/api/v1/cache/objects?associated=false"}
	for _, path := range paths {
		for _, tc := range []struct {
			user   string
			status int
		}{{"", 401}, {"gc", 403}, {"operator", 200}} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if tc.user != "" {
				r.SetBasicAuth(tc.user, "secret")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("GET %s as %s: status=%d body=%s", path, tc.user, w.Code, w.Body.String())
			}
		}
	}
	if guard.writes != 0 {
		t.Fatalf("GET wrote %d objects", guard.writes)
	}
	f.mu.Lock()
	after := f.requests
	f.mu.Unlock()
	if after != requests {
		t.Fatalf("GET contacted registry: before=%d after=%d", requests, after)
	}
	for path, status := range map[string]int{"/api/v1/images?category=invalid": 400, "/api/v1/images?scope=unsupported": 400, "/api/v1/cache/objects?associated=1": 400, "/api/v1/images?limit=1001": 400, "/api/v1/images/" + inventory.ID("absent"): 404, "/api/v1/images?snapshotID=" + inventory.ID("expired"): 410} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.SetBasicAuth("operator", "secret")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s status=%d want=%d body=%s", path, w.Code, status, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	r.SetBasicAuth("operator", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var spec struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if json.Unmarshal(w.Body.Bytes(), &spec) != nil || len(spec.Paths["/api/v1/images/{imageID}/objects"]) == 0 {
		t.Fatalf("missing inventory OpenAPI: %s", w.Body.String())
	}
	if snapshot.SnapshotID == "" {
		t.Fatal("missing snapshot")
	}
}

func TestInventoryLifecycleFailureDoesNotBlockRegistryReadAfterDurableExecutionFact(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	configDescriptor := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configDescriptor, Layers: []artifact.Descriptor{}})
	root := f.root("latest", manifest)
	refreshTestInventory(t, f.service)
	guard := &inventoryWriteGuard{Store: f.service.store, failLifecycle: true}
	f.service.inventory.Store = guard
	f.mu.Lock()
	before := f.requests
	f.mu.Unlock()
	_, _, err := f.processor.readDistributionManifest(context.Background(), &root, contentRef{digest: manifest.Digest, mediaType: manifest.MediaType, size: manifest.Size})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Registry read=%v", err)
	}
	f.mu.Lock()
	after := f.requests
	f.mu.Unlock()
	if after <= before {
		t.Fatal("inventory projection blocked Registry validation")
	}
	if err := backgroundUntilError(f.service, "execution"); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("projection failure=%v", err)
	}
	lifecycle, err := f.service.inventory.Lifecycle(context.Background(), inventory.LifecycleVersion(inventory.ID("platform", root.Authority, root.Repository, root.RootDigest), "", 0))
	if err != nil || lifecycle.Started == nil || *lifecycle.Started {
		t.Fatalf("failed start=%+v err=%v", lifecycle, err)
	}
	f.service.inventory.Store = f.service.store
	drainBackgroundWork(t, f.service, "execution")
	lifecycle, err = f.service.inventory.Lifecycle(context.Background(), lifecycle.Version)
	if err != nil || lifecycle.Started == nil || !*lifecycle.Started || lifecycle.FirstStartedAt == nil {
		t.Fatalf("recovered start=%+v err=%v", lifecycle, err)
	}
	if time.Since(f.service.inventoryInstalledAt) > time.Minute {
		t.Fatal("unexpected fixture runtime")
	}
}
