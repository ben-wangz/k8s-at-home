package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestSharedBlobDemandWakeRecoveryIsOrderedAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := NewRecords(store).EnsureBlobIntent(ctx, BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = NewRecords(store).SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	blobTask, created, err := manager.EnqueueBlob(ctx, intent)
	if err != nil || !created {
		t.Fatalf("initial blob task = %#v created=%t err=%v", blobTask, created, err)
	}

	rootA, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest-a", "sha256:"+strings.Repeat("b", 64), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	rootB, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest-b", "sha256:"+strings.Repeat("c", 64), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, demand := range []struct {
		id   string
		root Task
	}{
		{id: strings.Repeat("1", 32), root: rootA},
		{id: strings.Repeat("2", 32), root: rootB},
	} {
		if err := NewRecords(store).LinkBlobDemandScope(ctx, intent.ID, demand.id, demand.root.ID, "scope-"+demand.id, strings.Repeat("3", 32), 64); err != nil {
			t.Fatal(err)
		}
		if err := saveWakeTestTaskStatus(ctx, manager, demand.root.ID, Running, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Notifications arrive in reverse root order, and a duplicate delivery is harmless.
	if _, err := manager.Wake(ctx, rootB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wake(ctx, rootA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Wake(ctx, rootB.ID); err != nil {
		t.Fatalf("duplicate wake: %v", err)
	}
	for _, root := range []Task{rootA, rootB} {
		if _, err := store.Head(ctx, wakeRequestKey(root.ID)); err != nil {
			t.Fatalf("persisted wake for root %s: %v", root.ID, err)
		}
		if err := saveWakeTestTaskStatus(ctx, manager, root.ID, WaitingDeps, "dependencies_unavailable"); err != nil {
			t.Fatal(err)
		}
	}

	manager.Close()
	restarted, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.reconcileWakeRequests(ctx); err != nil {
		t.Fatalf("recover durable root wakes: %v", err)
	}
	for _, root := range []Task{rootA, rootB} {
		current, err := restarted.Get(ctx, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != Pending || current.WaitReason != "" {
			t.Fatalf("root %s after wake recovery = %q/%q, want pending", root.ID, current.Status, current.WaitReason)
		}
		if _, err := store.Head(ctx, wakeRequestKey(root.ID)); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("wake request for root %s after recovery = %v, want cleared", root.ID, err)
		}
	}

	demands, next, err := NewRecords(store).ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil || next != "" || len(demands) != 2 {
		t.Fatalf("shared blob demands = %d next=%q err=%v, want both owners retained", len(demands), next, err)
	}
	currentBlobTask, created, err := restarted.EnqueueBlob(ctx, intent)
	if err != nil || created || currentBlobTask.ID != blobTask.ID {
		t.Fatalf("duplicate blob enqueue = %#v created=%t err=%v, want existing single task %s", currentBlobTask, created, err, blobTask.ID)
	}
}

func saveWakeTestTaskStatus(ctx context.Context, manager *Manager, taskID string, status State, waitReason string) error {
	task, err := manager.Get(ctx, taskID)
	if err != nil {
		return err
	}
	task.Status = status
	task.WaitReason = waitReason
	if status == Running {
		task.Phase = "solidification"
	}
	currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+taskID, "wake-test", time.Minute)
	if err != nil {
		return err
	}
	if err := manager.SaveProgress(ctx, &task, currentLease); err != nil {
		_ = manager.Leases().Release(ctx, currentLease)
		return err
	}
	return manager.Leases().Release(ctx, currentLease)
}
