package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func TestQuotaCoveredWaitingIntentReusesExistingTask(t *testing.T) {
	f, task := quotaCoveredFixture(t)
	ctx := context.Background()
	intent, err := f.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentWaitingQuota, "", "")
	if err != nil {
		t.Fatal(err)
	}
	existing, created, err := f.service.tasks.EnqueueBlobProtected(ctx, intent, task.ScopeID, task.ReservedBytes)
	if err != nil || created || existing.ID != task.ID {
		t.Fatalf("existing quota task not reusable: %+v %v %v", existing, created, err)
	}
	if err = f.service.wakeCoveredBlob(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := f.service.tasks.Get(ctx, task.ID)
	if current.Status != tasks.Pending || current.ProtectionAssociatedAt.IsZero() {
		t.Fatalf("waiting intent did not resume: %+v", current)
	}
}

func TestQuotaWakeRejectsStaleFenceAndReplacedSession(t *testing.T) {
	f, task := quotaCoveredFixture(t)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	old, err := manager.Acquire(ctx, "quota-wake-safety", "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Release(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err = manager.Acquire(ctx, "quota-wake-safety", "new", time.Minute); err != nil {
		t.Fatal(err)
	}
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return manager.Validate(ctx, old) })
	if err = f.service.wakeCoveredBlob(guarded, task); !errors.Is(err, lease.ErrLost) {
		t.Fatalf("old fence woke task: %v", err)
	}
	if err = f.service.tasks.ReconcileProtectionState(ctx, task.ID, "replacement", []string{mustGateTestID(t)}, 64, 64); err != nil {
		t.Fatal(err)
	}
	if err = f.service.wakeCoveredBlob(ctx, task); err != nil {
		t.Fatal(err)
	}
	current, _ := f.service.tasks.Get(ctx, task.ID)
	if current.Status != tasks.WaitingQuota {
		t.Fatalf("stale session association woke task: %+v", current)
	}
}
