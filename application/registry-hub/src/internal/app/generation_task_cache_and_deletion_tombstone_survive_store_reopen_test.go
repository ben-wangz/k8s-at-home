package app

import (
	"bytes"
	"context"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestTaskCacheAndDeletionTombstoneSurviveStoreReopen(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	const authority, repository, reference = "ghcr.io", "team/image", "latest"
	service := &Service{store: store}
	generation, err := service.advanceGeneration(ctx, authority, repository, reference, false)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.Enqueue(ctx, authority, repository, reference)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("durable-root")
	digest := artifact.CalculateSHA256(body)
	if err := manager.SetDiscovered(ctx, &task, digest, ociManifestMediaType, generation.Generation); err != nil {
		t.Fatal(err)
	}
	task.Checkpoints = map[string]string{digest: "verified"}
	task.Graph = map[string]tasks.GraphEntry{digest: {Digest: digest, MediaType: ociManifestMediaType, Size: int64(len(body))}}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &task, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, taskLease); err != nil {
		t.Fatal(err)
	}
	cache := artifact.NewCache(store)
	if err := cache.Put(ctx, authority, repository, digest, ociManifestMediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if _, err := service.advanceGeneration(ctx, authority, repository, reference, true); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := filesystem.New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := tasks.NewManager(reopened, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	persisted, err := recovered.Get(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RootDigest != digest || persisted.Generation != generation.Generation || persisted.Checkpoints[digest] != "verified" || persisted.Graph[digest].Size != int64(len(body)) {
		t.Fatalf("recovered task lost root/checkpoint state: %#v", persisted)
	}
	cacheObject, err := artifact.NewCache(reopened).Get(ctx, authority, repository, digest, ociManifestMediaType)
	if err != nil {
		t.Fatalf("read complete cache after reopen: %v", err)
	}
	cacheBody, err := io.ReadAll(cacheObject.Body)
	_ = cacheObject.Body.Close()
	if err != nil || !bytes.Equal(cacheBody, body) {
		t.Fatalf("recovered complete cache = %q, read error=%v", cacheBody, err)
	}
	recoveredService := &Service{store: reopened}
	if err := recoveredService.checkTaskGeneration(ctx, &persisted); err == nil {
		t.Fatal("recovered stale task passed the persisted deletion tombstone")
	}
	record, err := recoveredService.generation(ctx, authority, repository, reference)
	if err != nil || !record.Tombstone || record.Generation != generation.Generation+1 {
		t.Fatalf("recovered deletion generation = %#v, err=%v", record, err)
	}
}
