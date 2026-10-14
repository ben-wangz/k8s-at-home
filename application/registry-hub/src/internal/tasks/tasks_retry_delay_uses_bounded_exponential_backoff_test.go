package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestRetryDelayUsesBoundedExponentialBackoff(t *testing.T) {
	for _, test := range []struct {
		retry int
		want  time.Duration
	}{
		{retry: -1, want: time.Second},
		{retry: 1, want: time.Second},
		{retry: 2, want: 2 * time.Second},
		{retry: 3, want: 4 * time.Second},
		{retry: 8, want: 128 * time.Second},
		{retry: 9, want: 256 * time.Second},
		{retry: 10, want: 256 * time.Second},
	} {
		if got := retryDelay(test.retry); got != test.want {
			t.Errorf("retryDelay(%d) = %s, want %s", test.retry, got, test.want)
		}
	}
}

func TestUnsupportedSchemaTaskIsNotOverwrittenOrCleaned(t *testing.T) {
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
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	unsupportedEntry := setTaskTestSchemaVersion(t, store, taskKey(task.ID), 99)
	if _, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest"); err == nil {
		t.Fatal("enqueue consumed a task with an unsupported schema")
	}
	if _, err := manager.Cleanup(ctx, 24*time.Hour, 100); err == nil {
		t.Fatal("cleanup accepted a task with an unsupported schema")
	}
	entry, err := store.Head(ctx, taskKey(task.ID))
	if err != nil {
		t.Fatalf("unsupported task was deleted: %v", err)
	}
	if entry.Version != unsupportedEntry.Version {
		t.Fatalf("unsupported task version changed: got %q, want %q", entry.Version, unsupportedEntry.Version)
	}
}

func TestUnsupportedSchemaInFlightIndexIsNotOverwritten(t *testing.T) {
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
	if _, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest"); err != nil {
		t.Fatal(err)
	}
	key := "tasks/inflight/" + inFlightKey("ghcr.io", "team/image", "latest")
	unsupportedEntry := setTaskTestSchemaVersion(t, store, key, 99)
	if _, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest"); err == nil {
		t.Fatal("enqueue consumed an in-flight index with an unsupported schema")
	}
	entry, err := store.Head(ctx, key)
	if err != nil {
		t.Fatalf("unsupported in-flight index was deleted: %v", err)
	}
	if entry.Version != unsupportedEntry.Version || taskTestSchemaVersion(t, store, key) != 99 {
		t.Fatal("unsupported in-flight index was changed")
	}
}

func TestCleanupRetainsRepairPendingTaskBeyondRetention(t *testing.T) {
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
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = RepairPending
	task.UpdatedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	removed, err := manager.Cleanup(ctx, 24*time.Hour, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("cleanup removed %d repair_pending task(s), want 0", removed)
	}
	if got, err := manager.Get(ctx, task.ID); err != nil || got.Status != RepairPending {
		t.Fatalf("repair_pending task after cleanup = %#v, err=%v", got, err)
	}
}

func TestRootPublicationFencesTaskCancellation(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "org/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = Running
	task.Phase = "solidification"
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(context.Background(), currentLease)

	if err := manager.BeginRootPublish(ctx, &task, currentLease); err != nil {
		t.Fatalf("BeginRootPublish() error = %v", err)
	}
	if _, err := manager.Cancel(ctx, task.ID); !errors.Is(err, ErrRootPublishing) {
		t.Fatalf("Cancel() error = %v, want root publication conflict", err)
	}
	persisted := mustGetTask(t, manager, task.ID)
	if !persisted.RootPublishing || persisted.CancelRequested {
		t.Fatalf("root publish barrier state = %#v", persisted)
	}
	if err := manager.MarkRootPublished(ctx, &task, currentLease); err != nil {
		t.Fatalf("MarkRootPublished() error = %v", err)
	}
	if _, err := manager.Cancel(ctx, task.ID); !errors.Is(err, ErrRootPublishing) {
		t.Fatalf("Cancel() after publication error = %v, want root publication conflict", err)
	}
	persisted = mustGetTask(t, manager, task.ID)
	if persisted.RootPublishing || !persisted.RootPublished || persisted.CancelRequested {
		t.Fatalf("published task state = %#v", persisted)
	}
}

func TestRootPublicationDoesNotStartAfterCancellation(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "org/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cancel(ctx, task.ID); err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(context.Background(), currentLease)
	if err := manager.BeginRootPublish(ctx, &task, currentLease); !errors.Is(err, ErrCancelled) {
		t.Fatalf("BeginRootPublish() error = %v, want cancellation", err)
	}
	persisted := mustGetTask(t, manager, task.ID)
	if persisted.RootPublishing || persisted.RootPublished {
		t.Fatalf("cancelled task entered root publication: %#v", persisted)
	}
}
