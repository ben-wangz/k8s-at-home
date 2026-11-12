package tasks

import (
	"context"
	"errors"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestWakeRequestGenerationAndConditionalConsumption(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m, err := NewManager(store, 1, 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	task, _, err := m.Enqueue(ctx, "ghcr.io", "team/image", "wake-generation")
	if err != nil {
		t.Fatal(err)
	}
	task.Status = Running
	if err = m.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err = m.ensureTaskWakeRequest(ctx, task); err != nil {
		t.Fatal(err)
	}
	old := task
	entry, err := store.Head(ctx, wakeRequestKey(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	task.Generation++
	if err = m.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err = m.ensureTaskWakeRequest(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err = m.clearObservedWakeRequest(ctx, task.ID, entry.Version); err != nil {
		t.Fatal(err)
	}
	if err = m.ensureTaskWakeRequest(ctx, old); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("old generation replaced wake: %v", err)
	}
	object, err := store.Get(ctx, wakeRequestKey(task.ID))
	if err != nil {
		t.Fatal(err)
	}
	var request wakeRequest
	if err = decodeRecord(object, 1<<20, &request); err != nil || request.Generation != task.Generation {
		t.Fatalf("fresh generation lost: %+v %v", request, err)
	}
	task.Status, task.WaitReason = WaitingQuota, "distribution_quota"
	if err = m.save(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if err = m.reconcileWakeRequests(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := m.Get(ctx, task.ID)
	if err != nil || current.Status != Pending {
		t.Fatalf("current wake not delivered: %+v %v", current, err)
	}
}
