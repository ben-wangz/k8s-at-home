package tasks

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/storage/filesystem"
)

func TestBackoffCancellationSurvivesLateWakeAndRestart(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "cancel-backoff")
	if err != nil {
		t.Fatal(err)
	}
	root.Status, root.Phase, root.RetryCount = RetryWait, "solidification", 5
	root.NextAttemptAt = time.Now().UTC().Add(time.Minute)
	if err := manager.save(ctx, &root); err != nil {
		t.Fatal(err)
	}
	if err := manager.ensureWakeRequest(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cancel(ctx, root.ID); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	manager, err = NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for range 3 {
		if err := manager.ensureWakeRequest(ctx, root.ID); err != nil {
			t.Fatal(err)
		}
		if err := manager.reconcileWakeRequests(ctx); err != nil {
			t.Fatal(err)
		}
		current, err := manager.Get(ctx, root.ID)
		if err != nil || !current.CancelRequested || current.Status != Cancelled || current.RootPublishing {
			t.Fatalf("late wake revived backoff cancellation: %#v error=%v", current, err)
		}
	}
}
