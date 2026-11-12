package tasks

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDispatchSkipsCandidateLeasedByAnotherInstance(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); manager.Close() }()
	first, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "first", "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "second", "sha256:"+strings.Repeat("b", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := manager.leases.Acquire(ctx, "tasks/"+first.ID, "another-instance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.leases.Release(context.Background(), holder)
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-processor.entered:
		if running.ID != second.ID {
			t.Fatalf("started task %s, want unleased task %s", running.ID, second.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leased first candidate blocked the unleased candidate")
	}
}

func TestCompetingDispatchersStartOneExecution(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 2)}
	first, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); first.Close(); second.Close() }()
	task, _, err := first.EnqueueResolved(ctx, "example.com", "repo", "competition", "sha256:"+strings.Repeat("d", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for _, manager := range []*Manager{first, second} {
		group.Add(1)
		go func() {
			defer group.Done()
			errs <- manager.dispatchPages(ctx, 0)
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case running := <-processor.entered:
		if running.ID != task.ID {
			t.Fatalf("started task %s, want %s", running.ID, task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("neither dispatcher started the task")
	}
	select {
	case duplicate := <-processor.entered:
		t.Fatalf("competing dispatchers started a duplicate execution: %s", duplicate.ID)
	case <-time.After(time.Second):
	}
}

func TestDispatchResumesAfterRecoveryPause(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); manager.Close() }()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "recovery", "sha256:"+strings.Repeat("e", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	manager.recoveryPaused.Store(true)
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-processor.entered:
		t.Fatalf("started task during recovery pause: %s", running.ID)
	default:
	}
	manager.recoveryPaused.Store(false)
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-processor.entered:
		if running.ID != task.ID {
			t.Fatalf("started task %s, want %s", running.ID, task.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task did not resume after recovery pause")
	}
}

func TestDispatchTakesOverRunningCandidateAfterLeaseRelease(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &dispatchBlockingProcessor{entered: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); manager.Close() }()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "running", "sha256:"+strings.Repeat("c", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = Running
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	holder, err := manager.leases.Acquire(ctx, "tasks/"+task.ID, "another-instance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-processor.entered:
		t.Fatalf("started task while another instance held its lease: %s", running.ID)
	default:
	}
	if err := manager.leases.Release(ctx, holder); err != nil {
		t.Fatal(err)
	}
	if err := manager.dispatchPages(ctx, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case running := <-processor.entered:
		if running.ID != task.ID {
			t.Fatalf("started task %s, want %s", running.ID, task.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("running task was not taken over after lease release")
	}
}
