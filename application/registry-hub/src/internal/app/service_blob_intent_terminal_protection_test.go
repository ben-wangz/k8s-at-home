package app

import (
	"context"
	"errors"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestRootCancellationRevokesOnlyItsBlobDemand(t *testing.T) {
	ctx := context.Background()
	_, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	rootA := newDemandRootTask(t, ctx, manager, "a")
	rootB := newDemandRootTask(t, ctx, manager, "b")
	linkRootBlobDemand(t, ctx, records, intent.ID, rootA.ID, strings.Repeat("1", 32), "scope-a", strings.Repeat("a", 32))
	linkRootBlobDemand(t, ctx, records, intent.ID, rootB.ID, strings.Repeat("2", 32), "scope-b", strings.Repeat("b", 32))

	if _, err := manager.Cancel(ctx, rootA.ID); err != nil {
		t.Fatalf("cancel first root: %v", err)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(demands) != 1 || demands[0].RootTaskID != rootB.ID {
		t.Fatalf("remaining blob demands = %#v, want only second root %q", demands, rootB.ID)
	}
	blobTask, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blobTask.Status != tasks.Pending {
		t.Fatalf("shared blob task status = %q, want pending", blobTask.Status)
	}
	cancelledRoot, err := manager.Get(ctx, rootA.ID)
	if err != nil || !cancelledRoot.TerminalCleanupComplete {
		t.Fatalf("cancelled root cleanup = %#v, err=%v; want completed", cancelledRoot, err)
	}
}

func TestRootCancellationCancelsBlobTaskAfterLastDemand(t *testing.T) {
	ctx := context.Background()
	_, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "c")
	linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, strings.Repeat("3", 32), "scope-c", strings.Repeat("c", 32))

	if _, err := manager.Cancel(ctx, root.ID); err != nil {
		t.Fatalf("cancel root: %v", err)
	}
	blobTask, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if blobTask.Status != tasks.Cancelled || !blobTask.TerminalCleanupComplete {
		t.Fatalf("undemanded blob task = %#v, want cancelled with completed cleanup", blobTask)
	}
	current, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || current.State != tasks.IntentCancelled {
		t.Fatalf("undemanded blob intent = %#v, err=%v; want cancelled", current, err)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil || len(demands) != 0 {
		t.Fatalf("remaining blob demands = %#v, err=%v; want none", demands, err)
	}
}

func TestSharedBlobCancellationKeepsProtectionAndAllowsNewRound(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	identity := tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64),
	}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := strings.Repeat("a", 32)
	if err := service.gate.AdmitProtection(ctx, sessionID, "blob:"+identity.Digest, intent.ID, 64, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	blobTask, created, err := service.enqueueBlobIntent(ctx, intent)
	if err != nil || !created {
		t.Fatalf("protected blob task = %#v created=%t err=%v", blobTask, created, err)
	}
	rootA := newDemandRootTask(t, ctx, manager, "shared-a")
	rootB := newDemandRootTask(t, ctx, manager, "shared-b")
	linkRootBlobDemand(t, ctx, records, intent.ID, rootA.ID, strings.Repeat("1", 32), "scope-a", strings.Repeat("b", 32))
	linkRootBlobDemand(t, ctx, records, intent.ID, rootB.ID, strings.Repeat("2", 32), "scope-b", strings.Repeat("c", 32))

	if _, err := manager.Cancel(ctx, rootA.ID); err != nil {
		t.Fatalf("cancel first root: %v", err)
	}
	demands, _, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil || len(demands) != 1 || demands[0].RootTaskID != rootB.ID {
		t.Fatalf("remaining shared demands = %#v err=%v, want second root only", demands, err)
	}
	retainedTask, err := manager.Get(ctx, intent.ID)
	if err != nil || retainedTask.Status == tasks.Cancelled {
		t.Fatalf("shared blob task after first cancel = %#v err=%v, want active", retainedTask, err)
	}
	sessions, _, err := service.gate.ListProtectionSessions(ctx, "", 10)
	if err != nil || len(sessions) != 1 || sessions[0].ID != sessionID || sessions[0].OwnerCount != 1 {
		t.Fatalf("shared protection after first cancel = %#v err=%v, want blob owner retained", sessions, err)
	}
	if err := service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission with remaining demand = %v, want protected", err)
	}

	if _, err := manager.Cancel(ctx, rootB.ID); err != nil {
		t.Fatalf("cancel last root: %v", err)
	}
	oldIntent, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || oldIntent.State != tasks.IntentCancelled {
		t.Fatalf("old blob intent after last cancellation = %#v err=%v", oldIntent, err)
	}
	if state := service.gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
		t.Fatalf("protection after last demand = %#v, want released", state)
	}

	next, created, err := records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil || !created || next.ID == intent.ID || next.Round != intent.Round+1 {
		t.Fatalf("new blob intent round = %#v created=%t err=%v", next, created, err)
	}
	next, err = records.SetBlobIntentReady(ctx, next.ID, "application/octet-stream", "cache-v2", 64)
	if err != nil {
		t.Fatal(err)
	}
	nextSessionID := strings.Repeat("d", 32)
	if err := service.gate.AdmitProtection(ctx, nextSessionID, "blob:"+identity.Digest, next.ID, 64, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := records.SetBlobIntentProtectionSession(ctx, next.ID, nextSessionID); err != nil {
		t.Fatal(err)
	}
	newBlobTask, created, err := service.enqueueBlobIntent(ctx, next)
	if err != nil || !created || newBlobTask.ID != next.ID || newBlobTask.Status != tasks.Pending {
		t.Fatalf("new protected blob task = %#v created=%t err=%v, want pending new round", newBlobTask, created, err)
	}
	if err := service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission after new round starts = %v, want protected", err)
	}
}

func TestBlobTerminalStateWakesWaitingRootDemand(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "terminal")
	root.Status = tasks.WaitingDeps
	root.WaitReason = "dependencies_unavailable"
	rootLease, err := manager.Leases().Acquire(ctx, "tasks/"+root.ID, "test-root", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, strings.Repeat("4", 32), "scope-terminal", strings.Repeat("c", 32))

	blobTask, err := manager.Get(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	blobTask.Status = tasks.Failed
	blobTask.ErrorClass = "distribution"
	blobLease, err := manager.Leases().Acquire(ctx, "tasks/"+blobTask.ID, "test-blob", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &blobTask, blobLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, blobLease); err != nil {
		t.Fatal(err)
	}
	processor := &solidifier{service: service, tasks: manager, gate: service.gate}
	if err := processor.TaskTerminal(ctx, &blobTask); err != nil {
		t.Fatalf("finish failed blob task: %v", err)
	}
	drainBackgroundWork(t, service, "notify", "wake_root")
	awakened, err := manager.Get(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if awakened.Status != tasks.Pending || awakened.WaitReason != "" {
		t.Fatalf("root after blob terminal event = %#v, want it pending", awakened)
	}
}
