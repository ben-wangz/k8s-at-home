package app

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestBlobEventPersistsWakeForRunningRoot(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "running")
	root.Status = tasks.Running
	rootLease, err := manager.Leases().Acquire(ctx, "tasks/"+root.ID, "test-root-running", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &root, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, strings.Repeat("5", 32), "scope-running", strings.Repeat("d", 32))

	service.wakeBlobDemandRoots(ctx, intent.ID)
	drainBackgroundWork(t, service, "notify", "wake_root")
	if _, err := service.store.Head(ctx, "tasks/wake/"+root.ID+".json"); err != nil {
		t.Fatalf("running root wake request: %v", err)
	}
}

func TestSharedBlobWakeNotifiesEveryWaitingRootIdempotently(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	rootA := newDemandRootTask(t, ctx, manager, "shared-wake-a")
	rootB := newDemandRootTask(t, ctx, manager, "shared-wake-b")
	demandIDs := []string{strings.Repeat("6", 32), strings.Repeat("7", 32)}
	for index, root := range []tasks.Task{rootA, rootB} {
		if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
			t.Fatal(err)
		}
		linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, demandIDs[index], "scope-shared-wake", strings.Repeat("e", 32))
	}

	service.wakeBlobDemandRoots(ctx, intent.ID)
	service.wakeBlobDemandRoots(ctx, intent.ID)
	drainBackgroundWork(t, service, "notify", "wake_root")
	for _, root := range []tasks.Task{rootA, rootB} {
		current, err := manager.Get(ctx, root.ID)
		if err != nil || current.Status != tasks.Pending || current.WaitReason != "" {
			t.Fatalf("root %s after repeated shared wake = %#v err=%v, want pending", root.ID, current, err)
		}
	}
	demands, next, err := records.ListBlobDemandRecords(ctx, intent.ID, "", 10)
	if err != nil || next != "" || len(demands) != 2 {
		t.Fatalf("shared demands after repeated wake = %#v next=%q err=%v, want both owners retained", demands, next, err)
	}
}

func newTerminalDemandFixture(t *testing.T) (*Service, *tasks.Manager, *tasks.Records) {
	t.Helper()
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, gate: gate, records: tasks.NewRecords(store)}
	processor := &solidifier{service: service, gate: gate}
	manager, err := tasks.NewManager(store, 1, 10, processor)
	if err != nil {
		t.Fatal(err)
	}
	processor.tasks = manager
	service.tasks = manager
	t.Cleanup(manager.Close)
	return service, manager, service.records
}

func newReadyBlobIntent(t *testing.T, ctx context.Context, manager *tasks.Manager, records *tasks.Records) tasks.BlobIntent {
	t.Helper()
	intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("d", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.EnqueueBlob(ctx, intent); err != nil {
		t.Fatal(err)
	}
	return intent
}

func newDemandRootTask(t *testing.T, ctx context.Context, manager *tasks.Manager, suffix string) tasks.Task {
	t.Helper()
	task, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest-"+suffix, "sha256:"+strings.Repeat("e", 64), "application/vnd.oci.image.manifest.v1+json", 1)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func saveDemandRootStatus(ctx context.Context, manager *tasks.Manager, taskID string, status tasks.State, waitReason string) error {
	task, err := manager.Get(ctx, taskID)
	if err != nil {
		return err
	}
	task.Status = status
	task.WaitReason = waitReason
	currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+taskID, "shared-demand-test", time.Minute)
	if err != nil {
		return err
	}
	if err := manager.SaveProgress(ctx, &task, currentLease); err != nil {
		_ = manager.Leases().Release(ctx, currentLease)
		return err
	}
	return manager.Leases().Release(ctx, currentLease)
}

func linkRootBlobDemand(t *testing.T, ctx context.Context, records *tasks.Records, intentID, rootTaskID, demandID, scopeID, sessionID string) {
	t.Helper()
	if err := records.LinkBlobDemandScope(ctx, intentID, demandID, rootTaskID, scopeID, sessionID, 64); err != nil {
		t.Fatal(err)
	}
}

func TestBlobIntentReconciliationClearsOnlyReleasedTerminalProtection(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := tasks.NewRecords(store)
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	ownerID := strings.Repeat("b", 32)
	sessionID := strings.Repeat("c", 32)
	if err := gate.AdmitProtection(ctx, sessionID, "scope", ownerID, 32, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := records.SetBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentAnchored, "", ""); err != nil {
		t.Fatal(err)
	}
	service := &Service{store: store, records: records, gate: gate}
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatal(err)
	}
	retained, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.ProtectionSessionID != sessionID {
		t.Fatalf("active terminal intent protection = %q, want %q", retained.ProtectionSessionID, sessionID)
	}
	if err := gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
		t.Fatal(err)
	}
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatal(err)
	}
	released, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.ProtectionSessionID != "" {
		t.Fatalf("released terminal intent protection = %q, want empty", released.ProtectionSessionID)
	}
}
