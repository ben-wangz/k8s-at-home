package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryCountAndCheckpointRecoverAfterManagerRestart(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	firstProcessor := &retryRecoveryProcessor{
		seen:    make(chan Task, 1),
		blocked: make(chan struct{}),
		err:     errors.New("temporary upstream failure"),
	}
	first, err := NewManager(store, 1, 10, firstProcessor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rootDigest := "sha256:" + strings.Repeat("a", 64)
	task, _, err := first.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetDiscovered(ctx, &task, rootDigest, "application/vnd.oci.image.manifest.v1+json"); err != nil {
		t.Fatal(err)
	}
	task.Checkpoints[rootDigest] = "verified"
	task.Graph[rootDigest] = GraphEntry{Digest: rootDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", Size: 123}
	if err := first.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	first.Start(ctx)
	select {
	case <-firstProcessor.seen:
	case <-time.After(2 * time.Second):
		first.Close()
		t.Fatal("initial worker did not start")
	}
	close(firstProcessor.blocked)
	waitForTaskState(t, first, task.ID, RetryWait)
	first.Close()

	persisted := mustGetTask(t, first, task.ID)
	if persisted.RetryCount != 1 || persisted.Checkpoints[rootDigest] != "verified" || persisted.NextAttemptAt.IsZero() {
		t.Fatalf("persisted retry state = retryCount %d nextAttemptAt %s checkpoints %#v", persisted.RetryCount, persisted.NextAttemptAt, persisted.Checkpoints)
	}
	persisted.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	if err := first.save(ctx, &persisted); err != nil {
		t.Fatal(err)
	}

	recoveryProcessor := &retryRecoveryProcessor{seen: make(chan Task, 1)}
	recovered, err := NewManager(store, 1, 10, recoveryProcessor)
	if err != nil {
		t.Fatal(err)
	}
	recovered.Start(ctx)
	select {
	case resumed := <-recoveryProcessor.seen:
		if resumed.RetryCount != 1 || resumed.Checkpoints[rootDigest] != "verified" {
			recovered.Close()
			t.Fatalf("recovered worker state = retryCount %d checkpoints %#v", resumed.RetryCount, resumed.Checkpoints)
		}
		if graphEntry, exists := resumed.Graph[rootDigest]; !exists || graphEntry.Digest != rootDigest || graphEntry.Size != 123 {
			recovered.Close()
			t.Fatalf("recovered graph entry = %#v exists=%t, want persisted root metadata", graphEntry, exists)
		}
	case <-time.After(2 * time.Second):
		recovered.Close()
		t.Fatal("recovered worker did not resume the persisted retry")
	}
	waitForTaskState(t, recovered, task.ID, Solidified)
	recovered.Close()
}

func TestYieldIsPersistedAsWaitingAndDoesNotConsumeRetry(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	processor := &yieldOnceProcessor{}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SetDiscovered(ctx, &task, "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json"); err != nil {
		t.Fatal(err)
	}
	manager.Start(ctx)
	defer manager.Close()

	waitForTaskState(t, manager, task.ID, WaitingCache)
	waiting := mustGetTask(t, manager, task.ID)
	if waiting.RetryCount != 0 || !waiting.NextAttemptAt.IsZero() || waiting.WaitReason != "cache_entry_unavailable" {
		t.Fatalf("yielded task = %#v, want durable wait without retry accounting", waiting)
	}
	if _, err := manager.Wake(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitForTaskState(t, manager, task.ID, Solidified)
	if calls := processor.calls.Load(); calls != 2 {
		t.Fatalf("processor calls = %d, want one wait and one completion", calls)
	}
}

type yieldOnceProcessor struct{ calls atomic.Int32 }

func (p *yieldOnceProcessor) Process(context.Context, *Task, lease.Lease) error {
	if p.calls.Add(1) == 1 {
		return WaitFor(WaitingCache, "cache_entry_unavailable")
	}
	return nil
}

func waitForTaskState(t *testing.T, manager *Manager, id string, expected State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		task, err := manager.Get(context.Background(), id)
		if err == nil && task.Status == expected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	task, err := manager.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("read task state: %v", err)
	}
	t.Fatalf("task status = %s, want %s", task.Status, expected)
}
