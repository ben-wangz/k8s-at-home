package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/tasks"
)

func TestBlobTerminalStateWakesProtectedCacheWaiterWithoutReservation(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)

	terminalIntent := newReadyBlobIntent(t, ctx, manager, records)
	terminalTask, err := manager.Get(ctx, terminalIntent.ID)
	if err != nil {
		t.Fatal(err)
	}
	terminalTask.Status = tasks.Solidified
	terminalLease, err := manager.Leases().Acquire(ctx, "tasks/"+terminalTask.ID, "test-terminal-blob", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &terminalTask, terminalLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, terminalLease); err != nil {
		t.Fatal(err)
	}
	if service.gate.HasProtectedCacheReservation(terminalTask.ID) {
		t.Fatal("terminal task unexpectedly owns a protected cache reservation")
	}

	waitingIntent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("8", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	waitingIntent, err = records.SetBlobIntentReady(ctx, waitingIntent.ID, "application/octet-stream", "cache-v1", 128)
	if err != nil {
		t.Fatal(err)
	}
	waitingTask, created, err := manager.EnqueueBlob(ctx, waitingIntent)
	if err != nil || !created {
		t.Fatalf("enqueue protected cache waiter: created=%t err=%v", created, err)
	}
	waitingTask.Status = tasks.WaitingQuota
	waitingTask.WaitReason = "protected_cache_capacity"
	waitingTask.QuotaWaitAt = time.Now().UTC()
	waitingTask.QuotaWaitReason = "protected_cache_capacity"
	waitingLease, err := manager.Leases().Acquire(ctx, "tasks/"+waitingTask.ID, "test-protected-cache-waiter", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &waitingTask, waitingLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, waitingLease); err != nil {
		t.Fatal(err)
	}

	processor := &solidifier{service: service, tasks: manager, gate: service.gate}
	if err := processor.TaskTerminal(ctx, &terminalTask); err != nil {
		t.Fatalf("finish terminal blob task: %v", err)
	}
	awakened, err := manager.Get(ctx, waitingTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if awakened.Status != tasks.Pending || awakened.WaitReason != "" {
		t.Fatalf("protected cache waiter after terminal task = %#v, want pending", awakened)
	}
}
