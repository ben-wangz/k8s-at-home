package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestInventoryRefillsFreeImageSlotsBeforeFinishingExistingCapture(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, 1)
	existing := &inventoryImageProgress{Phase: 7, Image: inventory.Image{ID: inventory.ID("existing"), Scope: "platform"}}
	state := inventoryCheckpoint{Build: &writer.Snapshot, ImageBuild: existing, ImagesEnrolled: true, BuildPage: &inventory.Cursor{}}
	for index := range 3 {
		identity := inventoryIdentity("ghcr.io", "team/refill", "")
		content := inventory.Content{Identity: identity, ObjectRef: inventory.ObjectRef{Digest: fmt.Sprintf("sha256:%064x", index+1), Role: "manifest", MediaType: ociManifestMediaType}, ClosureKnown: true}
		if err := f.service.inventory.SaveContent(ctx, content); err != nil {
			t.Fatal(err)
		}
		state.BuildPage.Entries = append(state.BuildPage.Entries, storage.Entry{Key: inventory.Prefix + "contents/" + identity.ContentID(content.Digest) + ".json"})
	}
	if err := f.service.buildInventoryPage(ctx, &state); err != nil {
		t.Fatal(err)
	}
	if state.ImageBuild != existing || len(state.ParallelImages) != 3 || !state.BuildPage.Done || existing.Capture.Phase != 0 {
		t.Fatalf("free slots were not refilled at a metadata barrier: parallel=%d cursor=%+v capture=%d", len(state.ParallelImages), state.BuildPage, existing.Capture.Phase)
	}
	for _, image := range append([]*inventoryImageProgress{state.ImageBuild}, state.ParallelImages...) {
		key := inventory.Prefix + "snapshots/" + writer.Snapshot.SnapshotID + "/captures/" + image.Image.ID + ".json"
		if _, err := f.service.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("refill wrote an immutable capture before checkpoint: %v", err)
		}
	}
	checkpoint, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	state = inventoryCheckpoint{}
	if err := json.Unmarshal(checkpoint, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.ParallelImages) != 3 || state.ImageBuild.Image.ID != existing.Image.ID || state.ImageBuild.Capture.Phase != 0 {
		t.Fatal("checkpoint replay lost enrolled image metadata")
	}
}

func TestInventoryAdoptedReferenceCursorDefersPublicationReconciliation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.inventoryDistributionChanged(ctx, "new-repository")
	state := inventoryCheckpoint{BuildPhase: 1, BuildPage: &inventory.Cursor{Token: "adopted-reference-cursor"}}
	if err := f.service.captureInventoryBuildPublications(ctx, &state); err != nil || !state.PublicationsCaptured || state.Publications != nil {
		t.Fatalf("adopted cursor attempted an incomplete publication pass: %+v error=%v", state, err)
	}
	if _, err := f.service.store.Head(ctx, inventoryChangeKey("distribution/repositories/new-repository")); err != nil {
		t.Fatalf("adopted cursor retired a publication it might not capture: %v", err)
	}
}
