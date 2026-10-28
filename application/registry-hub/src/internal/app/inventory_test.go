package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func enableInventory(f *stagedAcceptanceFixture) {
	f.service.inventory = &inventory.Repository{Store: f.service.store}
	f.service.cache.EnableInventoryTracking()
	f.service.initializeInventory(context.Background())
	f.service.cursorKey = []byte("test-inventory-cursor-key")
}

func refreshTestInventory(t *testing.T, s *Service) inventory.Snapshot {
	t.Helper()
	ctx := context.Background()
	scan := inventoryScan{}
	for n := 0; scan.Phase < 2; n++ {
		if n > 100 {
			t.Fatal("source scan did not converge")
		}
		if err := s.scanInventoryPage(ctx, &scan); err != nil {
			t.Fatal(err)
		}
	}
	scan.Phase = 3
	scan.Token = ""
	for n := 0; !scan.Complete; n++ {
		if n > 100 {
			t.Fatal("closure scan did not converge")
		}
		if err := s.scanInventoryPage(ctx, &scan); err != nil {
			t.Fatal(err)
		}
	}
	writer := s.inventory.NewWriter(time.Now().UTC(), time.Hour, 1)
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writer.Snapshot.GCEpoch = gate.Epoch
	state := inventoryCheckpoint{Build: &writer.Snapshot, Scan: scan}
	for n := 0; state.Build != nil; n++ {
		if n > 100 {
			t.Fatal("snapshot build did not converge")
		}
		if err := s.buildInventoryPage(ctx, &state); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := s.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func inventoryTestImage(t *testing.T, s *Service, snapshot inventory.Snapshot, id string) inventory.Image {
	t.Helper()
	image, err := s.inventory.Image(context.Background(), snapshot, id)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestInventoryCacheLifecycleAndAnchoredCleanup(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	config := f.cacheObject([]byte(`{"os":"linux","architecture":"amd64"}`), "application/vnd.oci.image.config.v1+json")
	layer := f.cacheObject([]byte("image layer"), "application/vnd.oci.image.layer.v1.tar")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{layer}})
	root := f.root("latest", manifest)
	identity := inventoryIdentity(root.Authority, root.Repository, root.Reference)
	if _, err := f.service.inventoryBinding(ctx, identity, manifest.Digest, "upstream", 0, time.Now().UTC(), true); err != nil {
		t.Fatal(err)
	}
	snapshot := refreshTestInventory(t, f.service)
	image := inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category != "cached_not_started" || image.Solidification.Started == nil || *image.Solidification.Started || image.Solidification.ExecutionState != "queued" {
		t.Fatalf("before worker=%+v", image)
	}
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) {
		t.Fatalf("root discovery=%v", err)
	}
	f.transfer(config)
	f.transfer(layer)
	if err := f.step(root.ID); err != nil {
		t.Fatal(err)
	}
	snapshot = refreshTestInventory(t, f.service)
	image = inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category != "solidified" || image.Solidification.Started == nil || !*image.Solidification.Started || !image.TagPublished {
		t.Fatalf("after solidification=%+v", image)
	}
	first := image.Solidification.FirstStartedAt
	if _, err := f.service.cache.Cleanup(ctx, time.Now().Add(48*time.Hour), 100); err != nil {
		t.Fatal(err)
	}
	snapshot = refreshTestInventory(t, f.service)
	image = inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category != "solidified" || image.Cache.State != "none" || first == nil || image.Solidification.FirstStartedAt == nil || !first.Equal(*image.Solidification.FirstStartedAt) {
		t.Fatalf("after cache cleanup=%+v", image)
	}
	if err := f.processor.PreserveTaskHistory(ctx, &root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.records.DeleteTaskGraph(ctx, root.ID, 1000); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.PreserveTaskHistory(ctx, &root); err != nil {
		t.Fatal(err)
	}
	snapshot = refreshTestInventory(t, f.service)
	image = inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category != "solidified" {
		t.Fatalf("graph cleanup lost inventory: %+v", image)
	}
	if _, err := f.service.advanceGeneration(ctx, root.Authority, root.Repository, layer.Digest, true); err != nil {
		t.Fatal(err)
	}
	snapshot = refreshTestInventory(t, f.service)
	image = inventoryTestImage(t, f.service, snapshot, identity.ID())
	if image.Category == "solidified" {
		t.Fatalf("deleted shared layer remains solidified: %+v", image)
	}
}

func TestInventoryPlatformIndependentOfMissingSiblingAndArtifact(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	config := f.cacheObject([]byte("arm config"), "application/vnd.oci.image.config.v1+json")
	arm := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	arm.Platform = json.RawMessage(`{"os":"linux","architecture":"arm64","variant":"v8"}`)
	absentConfig := artifact.Descriptor{Digest: artifact.CalculateSHA256([]byte("absent")), Size: 6, MediaType: "application/vnd.oci.image.config.v1+json"}
	missing := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &absentConfig, Layers: []artifact.Descriptor{}})
	missing.Platform = json.RawMessage(`{"os":"linux","architecture":"amd64"}`)
	attestation := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, ArtifactType: "application/example", Config: &absentConfig, Layers: []artifact.Descriptor{}})
	attestation.Platform = json.RawMessage(`{"os":"unknown","architecture":"unknown"}`)
	children := []artifact.Descriptor{arm, missing, attestation}
	index := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociIndexMediaType, Manifests: &children})
	root := f.root("multi", index)
	_ = f.step(root.ID)
	f.transfer(config)
	_ = f.step(root.ID)
	snapshot := refreshTestInventory(t, f.service)
	platform := inventoryTestImage(t, f.service, snapshot, inventory.ID("platform", root.Authority, root.Repository, arm.Digest))
	if platform.Category != "solidified" || platform.Platform == nil || platform.Platform.Architecture != "arm64" {
		t.Fatalf("arm platform=%+v", platform)
	}
	parent := inventoryTestImage(t, f.service, snapshot, inventoryIdentity(root.Authority, root.Repository, root.Reference).ID())
	if parent.Category == "solidified" || parent.Availability.State == "complete" {
		t.Fatalf("incomplete index=%+v", parent)
	}
	artifactImage := inventoryTestImage(t, f.service, snapshot, inventory.ID("platform", root.Authority, root.Repository, attestation.Digest))
	if artifactImage.Platform != nil || artifactImage.ArtifactType != "application/example" {
		t.Fatalf("artifact descriptor lost: %+v", artifactImage)
	}
}
