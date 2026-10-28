package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryBuilderResumesSourceCheckpointAndRepeatsFullScan(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/_catalog" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[]}`))
			return
		}
		f.serveHTTP(w, r)
	}))
	f.service.registry = registry
	f.service.cfg.Inventory = f.service.inventoryConfig()
	f.service.cfg.Inventory.BatchSize = 1
	config := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	f.root("latest", manifest)
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	state := inventoryCheckpoint{SchemaVersion: 1, Fence: leader.FencingToken}
	for n := 0; n < 2; n++ {
		if err := f.service.advanceInventory(ctx, leader, &state); err != nil {
			t.Fatal(err)
		}
	}
	restored, _, err := inventory.Read[inventoryCheckpoint](ctx, f.service.store, inventory.Prefix+"checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Scan.Phase != state.Scan.Phase || restored.Scan.Token != state.Scan.Token {
		t.Fatal("source cursor was not persisted")
	}
	for n := 0; n < 100; n++ {
		if err := f.service.advanceInventory(ctx, leader, &restored); err != nil {
			t.Fatal(err)
		}
		if restored.Build == nil && restored.Scan.Complete {
			break
		}
		if n == 99 {
			t.Fatal("build did not converge")
		}
	}
	snapshot, err := f.service.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || snapshot.Coverage != "complete" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	if len(snapshot.Reasons) == 0 {
		t.Fatal("historical coverage limit omitted")
	}
	restored.NextScanAt = time.Time{}
	if err := f.service.advanceInventory(ctx, leader, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Scan.Complete || restored.Build != nil {
		t.Fatal("new scan reused completion flag before finishing its sources")
	}
	stale := restored
	stale.Fence++
	if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"checkpoint.json", func(value *inventoryCheckpoint) error { *value = stale; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.service.advanceInventory(ctx, leader, &restored); !errors.Is(err, inventory.ErrSuperseded) {
		t.Fatalf("stale builder checkpoint write=%v", err)
	}
}

func TestInventoryPublicationAuditAndEpochInvalidation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	configBody := []byte(`{"os":"linux","architecture":"amd64"}`)
	config := artifact.Descriptor{Digest: artifact.CalculateSHA256(configBody), MediaType: "application/vnd.oci.image.config.v1+json", Size: int64(len(configBody))}
	manifest := artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}}
	body, _ := json.Marshal(manifest)
	digest := artifact.CalculateSHA256(body)
	f.objects[config.Digest] = stagedAcceptanceObject{body: configBody, mediaType: config.MediaType}
	f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: ociManifestMediaType}
	f.objects["pushed"] = f.objects[digest]
	identity := inventoryIdentity("", "local/image", "pushed")
	internal, err := inventoryInternalName(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.inventoryLocalTag(ctx, identity, internal); err != nil {
		t.Fatal(err)
	}
	snapshot := refreshTestInventory(t, f.service)
	image := inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category != "solidified" || image.Solidification.Started != nil {
		t.Fatalf("publication audit invented execution or missed closure: %+v", image)
	}
	gate := f.service.gate.State()
	gate.Epoch++
	if err := inventory.Update(ctx, f.service.store, "system/gc/state.json", func(value *gateRecord) error { *value = gate; return nil }); err != nil {
		t.Fatal(err)
	}
	delete(f.objects, config.Digest)
	snapshot = refreshTestInventory(t, f.service)
	image = inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category == "solidified" {
		t.Fatalf("old epoch proves a missing blob: %+v", image)
	}
	if _, _, err := inventory.Read[inventoryProof](ctx, f.service.store, inventory.Prefix+"proofs/"+identity.ContentID(digest)+".json"); errors.Is(err, storage.ErrNotFound) {
		t.Fatal("audit proof was not persisted")
	}
}
