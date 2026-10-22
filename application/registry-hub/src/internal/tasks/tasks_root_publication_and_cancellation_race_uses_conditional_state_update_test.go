package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
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
