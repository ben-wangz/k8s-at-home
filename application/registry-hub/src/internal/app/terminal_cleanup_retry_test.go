package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestTerminalCleanupFencesRetryAndDoesNotExpireAnExistingCleanup(t *testing.T) {
	f, _, root := terminalFixture(t, 1)
	ctx := context.Background()
	held, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+root.ID, "fail-root", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	root.Status, root.ErrorClass = tasks.Failed, "capacity_limit"
	if err := f.service.tasks.SaveProgress(ctx, &root, held); err != nil {
		t.Fatal(err)
	}
	if err := f.service.tasks.Leases().Release(ctx, held); err != nil {
		t.Fatal(err)
	}
	if err := f.processor.TaskTerminal(ctx, &root); err != nil {
		t.Fatal(err)
	}
	old := terminalJob(t, f.service, root)
	if _, err := f.service.tasks.Retry(ctx, root.ID); err == nil {
		t.Fatal("retry replaced a task while its old cleanup was pending")
	}
	if expired, err := f.service.tasks.CancelIfExpired(ctx, root.ID, time.Now().Add(48*time.Hour)); err != nil || expired {
		t.Fatalf("expiry changed an existing terminal cleanup identity: %t %v", expired, err)
	}
	drainBackgroundWork(t, f.service, "terminal_cleanup")
	current, err := f.service.tasks.Retry(ctx, root.ID)
	if err != nil || current.RoundID == root.RoundID || current.TerminalCleanupID != "" {
		t.Fatalf("completed cleanup prevented a fresh retry round: %+v %v", current, err)
	}
	_, done, err := f.service.terminalCleanupStep(ctx, &old)
	if err != nil || !done || !old.Superseded {
		t.Fatalf("old cleanup did not become superseded: done=%t error=%v", done, err)
	}
	latest, err := f.service.tasks.Get(ctx, root.ID)
	if err != nil || latest.Status != tasks.Pending || latest.RoundID != current.RoundID || latest.TerminalCleanupComplete {
		t.Fatalf("old cleanup modified the new round: %+v %v", latest, err)
	}
}

func TestTerminalCancelledBlobRejectsDemandBeforeIntentCleanup(t *testing.T) {
	service, manager, records := newTerminalDemandFixture(t)
	ctx := context.Background()
	intent := newReadyBlobIntent(t, ctx, manager, records)
	if _, err := manager.Cancel(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	root := newDemandRootTask(t, ctx, manager, "late-blob-demand")
	if err := records.LinkBlobDemandScope(ctx, intent.ID, mustGateTestID(t), root.ID, "scope", mustGateTestID(t), 64); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("new demand attached to a cancelling task: %v", err)
	}
	drainBackgroundWork(t, service, "terminal_cleanup")
	next, _, err := records.EnsureBlobIntent(ctx, intent.Identity, intent.Generation, "")
	if err != nil || next.ID == intent.ID {
		t.Fatalf("cleanup prevented a fresh intent round: %+v %v", next, err)
	}
}
