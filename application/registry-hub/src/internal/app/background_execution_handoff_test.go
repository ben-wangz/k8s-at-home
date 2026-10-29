package app

import (
	"context"
	"errors"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func TestExecutionHandoffPreservesDelayedSharedBlobFactsBeforeGraphRemoval(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	config := f.cacheObject([]byte(`{"os":"linux","architecture":"amd64"}`), "application/vnd.oci.image.config.v1+json")
	layer := f.cacheObject([]byte("handoff layer"), "application/vnd.oci.image.layer.v1.tar")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{layer}})
	root := f.root("handoff-delayed-execution", manifest)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) {
		t.Fatalf("discovery=%v", err)
	}
	f.transfer(config)
	f.transfer(layer)
	faults := &inventoryWriteGuard{Store: f.service.store, failLifecycle: true}
	f.service.inventory.Store = faults
	var err error
	for n := 0; n < 32; n++ {
		_, err = f.processor.PreserveTaskHistoryStep(ctx, &root)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("handoff projection failure=%v", err)
	}
	if _, err := f.service.records.GetGraphNode(ctx, root.ID, tasks.GraphManifest, manifest.Digest); err != nil {
		t.Fatalf("source lost before projection: %v", err)
	}
	f.service.inventory.Store = f.service.store
	// The failed unit stays visible until its independent retry is accepted.
	if _, err := backgroundTestPass(f.service, "execution"); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.PreserveTaskHistory(ctx, &root); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.records.DeleteTaskGraph(ctx, root.ID, 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.records.DeleteBlobDemandsForRoot(ctx, root.ID, 1000); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, f.service, "execution")
	identity := inventoryIdentity(root.Authority, root.Repository, root.Reference)
	binding, _, err := inventory.Read[inventory.Binding](ctx, f.service.store, inventory.Prefix+"bindings/"+identity.ID()+".json")
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := f.service.inventory.Lifecycle(ctx, inventory.LifecycleVersion(identity.ID(), binding.Revision, binding.ContentGeneration))
	if err != nil || lifecycle.Started == nil || !*lifecycle.Started || lifecycle.FirstStartedAt == nil {
		t.Fatalf("retained lifecycle=%+v err=%v", lifecycle, err)
	}
	q := workqueue.Queue{Store: f.service.store}
	base, err := q.Read(ctx, "execution", workqueue.ID("execution", executionSource(root.ID, manifest.Digest, 0)))
	if err != nil || !base.CreatedAt.Equal(*lifecycle.FirstStartedAt) {
		t.Fatalf("original execution time lost: fact=%+v lifecycle=%+v err=%v", base, lifecycle, err)
	}
}
