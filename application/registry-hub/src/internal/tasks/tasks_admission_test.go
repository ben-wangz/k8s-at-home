package tasks

import (
	"context"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type admissionGateProcessor struct {
	available atomic.Bool
	attempts  chan Task
	processed chan Task
	releases  atomic.Int32
}

func (p *admissionGateProcessor) AdmitTask(ctx context.Context, task *Task) (context.Context, func(), bool) {
	p.attempts <- *task
	if !p.available.Load() {
		return ctx, nil, false
	}
	return ctx, func() { p.releases.Add(1) }, true
}

func (p *admissionGateProcessor) Process(_ context.Context, task *Task, _ lease.Lease) error {
	p.processed <- *task
	return nil
}

func TestDispatchWaitsForProcessorAdmissionAndResumesOnWake(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	processor := &admissionGateProcessor{attempts: make(chan Task, 4), processed: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	manager.Start(ctx)
	defer func() { cancel(); manager.Close() }()
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "admission", "sha256:"+strings.Repeat("a", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-processor.attempts:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not attempt processor admission")
	}
	current := mustGetTask(t, manager, task.ID)
	if current.Status != Pending || current.RetryCount != 0 || current.AttemptID != "" || current.ErrorClass != "" {
		t.Fatalf("capacity rejection mutated task state: %#v", current)
	}
	select {
	case processed := <-processor.processed:
		t.Fatalf("processor ran without admission: %+v", processed)
	default:
	}

	processor.available.Store(true)
	manager.WakeDispatch()
	select {
	case processed := <-processor.processed:
		if processed.ID != task.ID || processed.Status != Running || processed.AttemptID == "" {
			t.Fatalf("processor received task before running transition: %+v", processed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch did not resume after processor capacity became available")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		current = mustGetTask(t, manager, task.ID)
		if current.Status == Solidified && processor.releases.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if current.Status != Solidified || current.RetryCount != 0 || processor.releases.Load() != 1 {
		t.Fatalf("task did not complete with one admission release: task=%+v releases=%d", current, processor.releases.Load())
	}
}

func TestAdmissionDefersStaleRunningTaskWithoutRetry(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	processor := &admissionGateProcessor{attempts: make(chan Task, 1), processed: make(chan Task, 1)}
	manager, err := NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueResolved(ctx, "example.com", "repo", "stale", "sha256:"+strings.Repeat("b", 64), "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = Running
	task.AttemptID = "previous-attempt"
	task.AttemptStartedAt = time.Now().UTC().Add(-time.Minute)
	task.RetryCount = 3
	task.Retryable = true
	task.ErrorClass = "previous_error"
	if err := manager.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	manager.process(ctx, 0, task)
	current := mustGetTask(t, manager, task.ID)
	if current.Status != Pending || current.RetryCount != 3 || current.AttemptID != "previous-attempt" || current.ErrorClass != "previous_error" {
		t.Fatalf("stale running task was counted as a failure or left running: %#v", current)
	}
	if current.AttemptFinishedAt.IsZero() {
		t.Fatal("abandoned attempt was left marked as running")
	}
	select {
	case <-processor.processed:
		t.Fatal("processor ran without capacity")
	default:
	}
}
