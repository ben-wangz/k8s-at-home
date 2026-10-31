package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestBlobDemandWakeDoesNotWaitForDurableNotificationQueue(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	waitingRoot := newDemandRootTask(t, ctx, manager, "immediate-wake")
	runningRoot := newDemandRootTask(t, ctx, manager, "immediate-wake-running")
	if err := saveDemandRootStatus(ctx, manager, waitingRoot.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := saveDemandRootStatus(ctx, manager, runningRoot.ID, tasks.Running, ""); err != nil {
		t.Fatal(err)
	}
	for index, root := range []tasks.Task{waitingRoot, runningRoot} {
		demandID := strings.Repeat(string(rune('a'+index)), 32)
		if err := records.LinkBlobDemandScope(ctx, intent.ID, demandID, root.ID, "scope-immediate", strings.Repeat("c", 32), 64); err != nil {
			t.Fatal(err)
		}
	}

	service.wakeBlobDemandRoots(ctx, intent.ID)

	currentWaiting, err := manager.Get(ctx, waitingRoot.ID)
	if err != nil || currentWaiting.Status != tasks.Pending || currentWaiting.WaitReason != "" {
		t.Fatalf("waiting root before notification queue drain = %#v err=%v, want pending", currentWaiting, err)
	}
	currentRunning, err := manager.Get(ctx, runningRoot.ID)
	if err != nil || currentRunning.Status != tasks.Running {
		t.Fatalf("running root before notification queue drain = %#v err=%v, want running", currentRunning, err)
	}
	if _, err := service.store.Head(ctx, "tasks/wake/"+runningRoot.ID+".json"); err != nil {
		t.Fatalf("running root wake request before notification queue drain: %v", err)
	}

	drainBackgroundWork(t, service, "notify", "wake_root")
	updatedIntent, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || !updatedIntent.WakeAcknowledged {
		t.Fatalf("durable notification after direct wake = %#v err=%v, want acknowledged", updatedIntent, err)
	}
}

func TestBlobCacheProtectionReferenceWakesRootWithoutDemandRecord(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	waitingRoot := newDemandRootTask(t, ctx, manager, "cache-protection-wake")
	foreignRoot, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "other/image", "latest-cache-protection-wake", waitingRoot.RootDigest, "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveDemandRootStatus(ctx, manager, waitingRoot.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
		t.Fatal(err)
	}
	if err := saveDemandRootStatus(ctx, manager, foreignRoot.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
		t.Fatal(err)
	}
	for _, root := range []tasks.Task{waitingRoot, foreignRoot} {
		if err := records.EnsureCacheProtectionReference(ctx, root.ID, root.Authority, root.Repository, intent.Identity.Digest); err != nil {
			t.Fatal(err)
		}
	}

	service.wakeBlobDemandRoots(ctx, intent.ID)

	current, err := manager.Get(ctx, waitingRoot.ID)
	if err != nil || current.Status != tasks.Pending || current.WaitReason != "" {
		t.Fatalf("root found only through cache protection index = %#v err=%v, want pending", current, err)
	}
	foreign, err := manager.Get(ctx, foreignRoot.ID)
	if err != nil || foreign.Status != tasks.WaitingDeps || foreign.WaitReason != "dependencies_unavailable" {
		t.Fatalf("root in another repository after digest-index wake = %#v err=%v, want unchanged", foreign, err)
	}
}

func TestImmediateBlobDemandWakeRetainsDurableFallbackOnWakeFailure(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "immediate-wake-fallback")
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.Running, ""); err != nil {
		t.Fatal(err)
	}
	if err := records.LinkBlobDemandScope(ctx, intent.ID, strings.Repeat("d", 32), root.ID, "scope-immediate-fallback", strings.Repeat("e", 32), 64); err != nil {
		t.Fatal(err)
	}

	faults := &blobWakeFaultStore{Store: service.store, rejectWake: true}
	manager.Close()
	replacement, err := tasks.NewManager(faults, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	service.store = faults
	service.records = tasks.NewRecords(faults)
	service.tasks = replacement

	service.wakeBlobDemandRoots(ctx, intent.ID)
	if faults.failedWakes != 1 {
		t.Fatalf("immediate wake attempts = %d, want 1", faults.failedWakes)
	}
	if _, err := faults.Head(ctx, "tasks/wake/"+root.ID+".json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("wake request after injected failure = %v, want absent", err)
	}
	if err := backgroundUntilError(service, "notify", "wake_root"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("durable wake after injected failure = %v, want retryable failure", err)
	}
	faults.rejectWake = false
	drainBackgroundWork(t, service, "notify", "wake_root")
	if _, err := faults.Head(ctx, "tasks/wake/"+root.ID+".json"); err != nil {
		t.Fatalf("durable fallback did not persist the running-root wake request: %v", err)
	}
}
