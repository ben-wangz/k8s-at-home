package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRootPublicationFenceAndCancellationIntentAreOrdered(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	manager, err := NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for index := range 20 {
		task, _, err := manager.Enqueue(ctx, "ghcr.io", "org/image", fmt.Sprintf("tag-%d", index))
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

		start := make(chan struct{})
		type operationResult struct {
			begin bool
			err   error
		}
		results := make(chan operationResult, 2)
		go func() {
			<-start
			copy := task
			results <- operationResult{begin: true, err: manager.BeginRootPublish(ctx, &copy, currentLease)}
		}()
		go func() {
			<-start
			_, err := manager.Cancel(ctx, task.ID)
			results <- operationResult{err: err}
		}()
		close(start)
		var beginErr, cancelErr error
		for range 2 {
			result := <-results
			if result.begin {
				beginErr = result.err
			} else {
				cancelErr = result.err
			}
		}
		if releaseErr := manager.Leases().Release(ctx, currentLease); releaseErr != nil {
			t.Fatal(releaseErr)
		}
		current, err := manager.Get(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !current.CancelRequested {
			t.Fatalf("cancellation intent was not persisted: begin=%v cancel=%v state=%#v", beginErr, cancelErr, current)
		}
		if current.RootPublishing && beginErr != nil {
			t.Fatalf("root publication fence persisted despite a failed begin: %v state=%#v", beginErr, current)
		}
		if !current.RootPublishing && !errors.Is(beginErr, ErrCancelled) && !errors.Is(beginErr, storage.ErrConditionFailed) {
			t.Fatalf("publication proceeded after cancellation won: begin=%v state=%#v", beginErr, current)
		}
		if cancelErr != nil {
			t.Fatalf("cancellation failed: %v", cancelErr)
		}
	}
}

func TestRepairCompleteResumesTaskUntilRootIsPublished(t *testing.T) {
	for _, rootPublished := range []bool{false, true} {
		name := "resume unfinished root"
		if rootPublished {
			name = "complete published root"
		}
		t.Run(name, func(t *testing.T) {
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
			task.RootDigest = "sha256:" + strings.Repeat("a", 64)
			task.Status = RepairPending
			task.Phase = "solidification"
			task.RootPublishing = true
			task.RootPublished = rootPublished
			if err := manager.save(ctx, &task); err != nil {
				t.Fatal(err)
			}

			if err := manager.RepairComplete(ctx, task.ID); err != nil {
				t.Fatalf("RepairComplete() error = %v", err)
			}
			current, err := manager.Get(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.RootPublishing || current.RootPublished != rootPublished {
				t.Fatalf("root publication state = publishing %t published %t", current.RootPublishing, current.RootPublished)
			}
			if rootPublished {
				if current.Status != Solidified {
					t.Fatalf("status = %q, want solidified", current.Status)
				}
				if _, err := manager.getInFlight(ctx, inFlightKey(current.Authority, current.Repository, current.Reference)); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("completed task in-flight lookup error = %v, want not found", err)
				}
				return
			}
			if current.Status != Pending || current.Phase != "solidification" {
				t.Fatalf("unfinished root task = %#v, want pending solidification", current)
			}
			if _, err := manager.getInFlight(ctx, inFlightKey(current.Authority, current.Repository, current.Reference)); err != nil {
				t.Fatalf("unfinished task lost its in-flight queue entry: %v", err)
			}
		})
	}
}

func mustGetTask(t *testing.T, manager *Manager, id string) Task {
	t.Helper()
	task, err := manager.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

type retryRecoveryProcessor struct {
	seen    chan Task
	blocked chan struct{}
	err     error
}

func (p *retryRecoveryProcessor) Process(_ context.Context, task *Task, _ lease.Lease) error {
	p.seen <- *task
	if p.blocked != nil {
		<-p.blocked
	}
	return p.err
}

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
