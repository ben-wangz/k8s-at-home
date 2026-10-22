package tasks

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestWaitingRootDoesNotConsumeRunnableBlobQueueCapacity(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()

	root, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	root.Status = WaitingDeps
	root.Phase = "solidification"
	root.WaitReason = "blob_missing"
	if err := manager.save(ctx, &root); err != nil {
		t.Fatal(err)
	}

	intentID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	intent := BlobIntent{
		ID: intentID, State: IntentReady, MediaType: "application/octet-stream", CacheVersion: "cache-version",
		Identity:   BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64)},
		ActualSize: 10,
	}
	if _, created, err := manager.EnqueueBlob(ctx, intent); err != nil || !created {
		t.Fatalf("EnqueueBlob() created=%t err=%v; waiting root must not block runnable capacity", created, err)
	}
}

func TestRunnableRootConsumesBlobQueueCapacity(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()

	root, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	root.Phase = "solidification"
	root.RootDigest = "sha256:" + strings.Repeat("b", 64)
	if err := manager.save(ctx, &root); err != nil {
		t.Fatal(err)
	}

	intentID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	intent := BlobIntent{
		ID: intentID, State: IntentReady, MediaType: "application/octet-stream", CacheVersion: "cache-version",
		Identity:   BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("c", 64)},
		ActualSize: 10,
	}
	if _, _, err := manager.EnqueueBlob(ctx, intent); err == nil {
		t.Fatal("EnqueueBlob() accepted work after runnable queue capacity was full")
	}
}

func TestEnqueueBlobPersistsTransferredProtectionSessionBeforeScheduling(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	intentID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := randomID()
	if err != nil {
		t.Fatal(err)
	}
	intent := BlobIntent{
		ID: intentID, State: IntentReady, MediaType: "application/octet-stream", CacheVersion: "cache-version",
		Identity:   BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64)},
		ActualSize: 10, ProtectionSessionID: sessionID,
	}
	task, created, err := manager.EnqueueBlobProtected(context.Background(), intent, "manifest-scope", 64)
	if err != nil || !created {
		t.Fatalf("EnqueueBlob() = %#v, %t, %v", task, created, err)
	}
	if task.ProtectionSessionID != sessionID || task.ScopeID != "manifest-scope" || task.RequestedBytes != 64 || task.ReservedBytes != 64 || len(task.SessionIDs) != 1 || task.SessionIDs[0] != sessionID || !task.GCProtected {
		t.Fatalf("scheduled blob protection = %#v, want transferred session persisted", task)
	}
	persisted, err := manager.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatalf("read persisted protected blob task: %v", err)
	}
	if persisted.ScopeID != task.ScopeID || persisted.ReservedBytes != task.ReservedBytes || !persisted.GCProtected {
		t.Fatalf("persisted blob protection = %#v, want atomic scope and reservation", persisted)
	}
}

func TestRetryCapacityLimitStartsFreshRootRound(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	rootDigest := "sha256:" + strings.Repeat("d", 64)
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", rootDigest, "application/vnd.oci.image.index.v1+json", 4)
	if err != nil {
		t.Fatal(err)
	}
	oldRound := task.RoundID
	task.Status = Failed
	task.ErrorClass = "capacity_limit"
	task.Retryable = false
	task.Graph[rootDigest] = GraphEntry{Digest: rootDigest, MediaType: task.MediaType, Size: 100}
	task.Checkpoints[rootDigest] = "verified"
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}

	retried, err := manager.Retry(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != Pending || retried.RoundID == "" || retried.RoundID == oldRound || retried.ErrorClass != "" || retried.Discovery != "manifest" {
		t.Fatalf("retried task = %#v, want a clean new round", retried)
	}
	if len(retried.Graph) != 0 || len(retried.Checkpoints) != 0 || retried.RootDigest != rootDigest || retried.Generation != 4 {
		t.Fatalf("new round retained stale graph or lost root identity: %#v", retried)
	}
}
