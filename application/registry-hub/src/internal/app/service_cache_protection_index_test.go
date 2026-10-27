package app

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestCacheProtectionIndexTracksUnanchoredManifestNodes(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	records := tasks.NewRecords(store)
	service := &Service{tasks: manager, records: records, store: store, gate: gate}
	digest := "sha256:" + strings.Repeat("a", 64)
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", digest, "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := service.generation(ctx, task.Authority, task.Repository, digest)
	if err != nil {
		t.Fatal(err)
	}
	node := tasks.GraphNode{
		RootTaskID: task.ID, Role: tasks.GraphManifest, Digest: digest,
		MediaType: "application/vnd.oci.image.manifest.v1+json", DeclaredSize: 128,
		LocalState: "discovered", Generation: task.Generation,
		ContentGeneration: &generation.Generation, GCEpoch: gate.State().Epoch,
	}
	if err := records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphManifest,
		Digest: digest, ActualSize: node.ActualSize, Generation: generation.Generation,
		GCEpoch: gate.State().Epoch, OperationID: "cache-protection-manifest-test",
		VerificationBasis: "distribution_readback",
	}); err != nil {
		t.Fatal(err)
	}
	if err := records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, digest); err != nil {
		t.Fatal(err)
	}
	if err := records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.BeginRootPublish(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.MarkRootPublished(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, taskLease); err != nil {
		t.Fatal(err)
	}
	service.cacheProtectionReady.Store(true)
	protected, err := service.protectCacheObject(ctx, "ghcr.io", "team/image", digest)
	if err != nil || !protected {
		t.Fatalf("unanchored manifest cache protection = %t, %v; want protected", protected, err)
	}

	node.LocalState = "anchored"
	if err := records.SaveGraphNode(ctx, &node); err != nil {
		t.Fatal(err)
	}
	protected, err = service.protectCacheObject(ctx, "ghcr.io", "team/image", digest)
	if err != nil || protected {
		t.Fatalf("anchored manifest cache protection = %t, %v; want unprotected", protected, err)
	}

	blobDigest := "sha256:" + strings.Repeat("b", 64)
	if err := records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, blobDigest); err != nil {
		t.Fatal(err)
	}
	blobNode := tasks.GraphNode{
		RootTaskID: task.ID, Role: tasks.GraphBlob, Digest: blobDigest,
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", DeclaredSize: 256,
		LocalState: "discovered", Generation: task.Generation,
	}
	if err := records.SaveGraphNode(ctx, &blobNode); err != nil {
		t.Fatal(err)
	}
	blobGeneration, err := service.generation(ctx, task.Authority, task.Repository, blobDigest)
	if err != nil {
		t.Fatal(err)
	}
	blobNode.ContentGeneration = &blobGeneration.Generation
	blobNode.GCEpoch = gate.State().Epoch
	if err := records.SaveGraphNode(ctx, &blobNode); err != nil {
		t.Fatal(err)
	}
	if err := records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphBlob,
		Digest: blobDigest, ActualSize: blobNode.ActualSize, Generation: blobGeneration.Generation,
		GCEpoch: gate.State().Epoch, OperationID: "cache-protection-test",
		VerificationBasis: "distribution_readback",
	}); err != nil {
		t.Fatal(err)
	}
	intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: task.Authority, Repository: task.Repository, Digest: blobDigest,
	}, task.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentFailed, "upstream", "unavailable"); err != nil {
		t.Fatal(err)
	}
	protected, err = service.protectCacheObject(ctx, "ghcr.io", "team/image", blobDigest)
	if err != nil || !protected {
		t.Fatalf("failed blob intent cache protection = %t, %v; want protected by the active root", protected, err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentAnchored, "", ""); err != nil {
		t.Fatal(err)
	}
	protected, err = service.protectCacheObject(ctx, "ghcr.io", "team/image", blobDigest)
	if err != nil || !protected {
		t.Fatalf("anchored intent with discovered graph node protection = %t, %v; want protected", protected, err)
	}
	blobNode.LocalState = "anchored"
	if err := records.SaveGraphNode(ctx, &blobNode); err != nil {
		t.Fatal(err)
	}
	protected, err = service.protectCacheObject(ctx, "ghcr.io", "team/image", blobDigest)
	if err != nil || protected {
		t.Fatalf("verified anchored blob cache protection = %t, %v; want unprotected", protected, err)
	}
}
