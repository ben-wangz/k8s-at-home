package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryClientPublicationForcesDurableCatalogReconciliation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.pushSlots = make(chan struct{}, 1)
	ctx := context.Background()
	routed, err := route.Parse("/v2/ghcr.io/team/image/manifests/latest")
	if err != nil {
		t.Fatal(err)
	}
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" || r.URL.Path == "/v2/"+routed.Internal+"/tags/list" {
			f.mu.Lock()
			_, published := f.objects["latest"]
			f.mu.Unlock()
			values := []string{}
			field := "repositories"
			if r.URL.Path == "/v2/_catalog" {
				if published && r.URL.Query().Get("last") == "" {
					values = append(values, routed.Internal)
				}
			} else {
				field = "tags"
				if published {
					values = append(values, "latest")
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string][]string{field: values})
			return
		}
		f.serveHTTP(w, r)
	}))
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{SchemaVersion: 1, Fence: leader.FencingToken}
	advance := func() {
		t.Helper()
		if err := f.service.advanceInventory(ctx, leader, &state); err != nil {
			t.Fatal(err)
		}
		state, _, err = inventory.Read[inventoryCheckpoint](ctx, f.service.store, inventory.Prefix+"checkpoint.json")
		if err != nil {
			t.Fatal(err)
		}
		// Model another replica resuming solely from durable checkpoints and notifications.
		f.service.inventoryDirty.Store(false)
		f.service.inventoryReconcile.Store(false)
	}
	for step := 0; state.LastPublishedAt.IsZero(); step++ {
		if step == 1000 {
			t.Fatal("initial inventory did not converge")
		}
		advance()
	}
	if !state.NextReconcileAt.After(time.Now().Add(4 * time.Minute)) {
		t.Fatal("test requires a future periodic reconciliation deadline")
	}
	configBody := []byte(`{"os":"linux","architecture":"amd64"}`)
	config := artifact.Descriptor{Digest: artifact.CalculateSHA256(configBody), MediaType: "application/vnd.oci.image.config.v1+json", Size: int64(len(configBody))}
	body, _ := json.Marshal(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	digest := artifact.CalculateSHA256(body)
	f.objects[config.Digest] = stagedAcceptanceObject{body: configBody, mediaType: config.MediaType}
	f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: ociManifestMediaType}
	request := httptest.NewRequest(http.MethodPut, "/v2/ghcr.io/team/image/manifests/latest", bytes.NewReader(body))
	request.Header.Set("Content-Type", ociManifestMediaType)
	response := httptest.NewRecorder()
	f.service.pushManifest(response, request, routed, url.Values{}, &requestMeta{})
	if response.Code != http.StatusCreated {
		t.Fatalf("manifest push: status=%d body=%s", response.Code, response.Body.String())
	}
	change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey("distribution/catalog"))
	if err != nil || change.Revision == "" {
		t.Fatalf("publication notification missing: %+v error=%v", change, err)
	}
	state.NextScanAt = time.Time{}
	initialPublication := state.LastPublishedAt
	sawBarrier := false
	for step := 0; state.LastPublishedAt.Equal(initialPublication); step++ {
		if step == 1000 {
			t.Fatal("publication was not discovered before periodic reconciliation")
		}
		advance()
		_, _, markerErr := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(change.Key))
		if state.NextReconcileAt.IsZero() && markerErr == nil {
			sawBarrier = true
		}
		if errors.Is(markerErr, storage.ErrNotFound) && !sawBarrier {
			t.Fatal("publication notification retired before checkpointing reconciliation")
		}
	}
	if !sawBarrier {
		t.Fatal("reconciliation checkpoint barrier was not exercised")
	}
	snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	image := inventoryTestImage(t, f.service, snapshot, inventoryIdentity("ghcr.io", "team/image", "latest").ID())
	if image.Category != "solidified" || image.ResolvedDigest != digest || image.Availability.State != "complete" {
		t.Fatalf("published manifest missing from refreshed inventory: %+v", image)
	}
}

func TestInventoryCatalogMarkerFailureCheckpointsReconciliationBeforeAcknowledgement(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.inventoryDistributionChanged(ctx)
	change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey("distribution/catalog"))
	if err != nil {
		t.Fatal(err)
	}
	observed := f.service.store.(*observedStore)
	original := observed.Store
	observed.Store = inventoryMarkerFailureStore{Store: original}
	f.service.inventoryDistributionChanged(ctx)
	observed.Store = original
	if !f.service.inventoryDirty.Load() || !f.service.inventoryReconcile.Load() {
		t.Fatal("failed catalog notification did not request reconciliation")
	}
	state := inventoryCheckpoint{Scan: inventoryScan{Complete: true}, NextReconcileAt: time.Now().Add(time.Hour),
		Delta: &inventoryDeltaProgress{Phase: 3, Changes: []inventoryChange{change}, Cursor: inventory.Cursor{Done: true}}}
	if ready, err := f.service.advanceInventorySources(ctx, &state); err != nil || ready || !state.NextReconcileAt.IsZero() {
		t.Fatalf("reconciliation requirement missing: ready=%t state=%+v error=%v", ready, state, err)
	}
	if _, _, err := inventory.Read[inventoryChange](ctx, f.service.store, inventoryChangeKey(change.Key)); err != nil {
		t.Fatalf("notification retired before checkpoint barrier: %v", err)
	}
	data, _ := json.Marshal(state)
	state = inventoryCheckpoint{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if ready, err := f.service.advanceInventorySources(ctx, &state); err != nil || ready || state.Scan.Complete || state.Delta != nil {
		t.Fatalf("restored requirement did not start full reconciliation: ready=%t state=%+v error=%v", ready, state, err)
	}
}
