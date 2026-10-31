package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestBlobReconciliationRecoversWakeFailureBeforeDurableRequest(t *testing.T) {
	for _, state := range []tasks.BlobIntentState{tasks.IntentReady, tasks.IntentVerified, tasks.IntentAnchored, tasks.IntentFailed, tasks.IntentCancelled} {
		t.Run(string(state), func(t *testing.T) {
			testBlobWakeRecovery(t, state)
		})
	}
}

func testBlobWakeRecovery(t *testing.T, state tasks.BlobIntentState) {
	ctx := context.Background()
	service, _, _ := newTerminalDemandFixture(t)
	faults := &blobWakeFaultStore{Store: service.store}
	manager, err := tasks.NewManager(faults, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	service.store = faults
	service.tasks = manager
	service.records = tasks.NewRecords(faults)
	intent := newReadyBlobIntent(t, ctx, manager, service.records)
	root := newDemandRootTask(t, ctx, manager, "wake-write-failure")
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.Running, ""); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, service.records, intent.ID, root.ID, strings.Repeat("8", 32), "scope-wake-failure", strings.Repeat("9", 32))
	intent, err = service.records.SetBlobIntentState(ctx, intent.ID, state, "", "")
	if err != nil {
		t.Fatal(err)
	}
	faults.rejectWake = true
	service.wakeBlobDemandRoots(ctx, intent.ID)
	if err := backgroundUntilError(service, "notify", "wake_root"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("wake delivery failure=%v", err)
	}
	if faults.failedWakes != 2 {
		t.Fatalf("rejected immediate and durable wake writes = %d, want 2", faults.failedWakes)
	}
	if _, err := faults.Head(ctx, "tasks/wake/"+root.ID+".json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("wake request after failed write = %v, want absent", err)
	}
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
		t.Fatal(err)
	}
	manager.Close()
	faults.rejectWake = false
	restarted, err := tasks.NewManager(faults, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	service.tasks = restarted
	service.records = tasks.NewRecords(faults)
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatalf("reconcile completed blob after restart: %v", err)
	}
	drainBackgroundWork(t, service, "notify", "wake_root")
	current, err := restarted.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.Pending || current.WaitReason != "" {
		t.Fatalf("root after lost-wake recovery = %#v err=%v, want pending", current, err)
	}
	recovered, err := service.records.GetBlobIntent(ctx, intent.ID)
	if err != nil || !recovered.WakeAcknowledged || !recovered.LastProgressAt.Equal(intent.LastProgressAt) || !recovered.ExpiresAt.Equal(intent.ExpiresAt) {
		t.Fatalf("acknowledged intent = %#v err=%v, want delivered wake without renewed idle lifetime", recovered, err)
	}
	writes := faults.intentWrites
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if faults.intentWrites != writes {
		t.Fatal("acknowledged notification was written again during reconciliation")
	}
}

func TestBlobWakeAcknowledgementFailureIsReplayedAfterRestart(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "wake-ack-failure")
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.WaitingDeps, "dependencies_unavailable"); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, strings.Repeat("a", 32), "scope-ack-failure", strings.Repeat("b", 32))
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	faults := &blobWakeFaultStore{Store: service.store, rejectIntent: true}
	service.records = tasks.NewRecords(faults)
	if err := service.notifyBlobDemandRoots(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	if err := backgroundUntilError(service, "notify", "wake_root"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("notification acknowledgement=%v", err)
	}
	current, err := manager.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.Pending {
		t.Fatalf("root before acknowledgement = %#v err=%v, want wake already delivered", current, err)
	}
	pending, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || pending.WakeAcknowledged {
		t.Fatalf("failed acknowledgement = %#v err=%v, want pending replay", pending, err)
	}
	service.records = tasks.NewRecords(service.store)
	restarted, err := tasks.NewManager(service.store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	service.tasks = restarted
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, service, "notify", "wake_root")
	recovered, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || !recovered.WakeAcknowledged {
		t.Fatalf("replayed acknowledgement = %#v err=%v", recovered, err)
	}
}

func TestBlobWakeAcknowledgementCannotOverwriteConcurrentState(t *testing.T) {
	ctx := context.Background()
	service, manager, records := newTerminalDemandFixture(t)
	intent := newReadyBlobIntent(t, ctx, manager, records)
	root := newDemandRootTask(t, ctx, manager, "wake-state-race")
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.Running, ""); err != nil {
		t.Fatal(err)
	}
	linkRootBlobDemand(t, ctx, records, intent.ID, root.ID, strings.Repeat("c", 32), "scope-state-race", strings.Repeat("d", 32))
	if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentVerified, "", ""); err != nil {
		t.Fatal(err)
	}
	faults := &blobWakeFaultStore{Store: service.store}
	faults.afterWake = func() {
		if _, err := records.SetBlobIntentState(ctx, intent.ID, tasks.IntentAnchored, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	replacement, err := tasks.NewManager(faults, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replacement.Close)
	service.tasks = replacement
	if err := service.notifyBlobDemandRoots(ctx, intent.ID); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, service, "notify", "wake_root")
	current, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil || current.State != tasks.IntentAnchored || current.WakeAcknowledged {
		t.Fatalf("concurrent state = %#v err=%v, want anchored with its notification pending", current, err)
	}
	if err := service.reconcileBlobIntentTasks(ctx); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, service, "notify", "wake_root")
	current, err = records.GetBlobIntent(ctx, intent.ID)
	if err != nil || current.State != tasks.IntentAnchored || !current.WakeAcknowledged {
		t.Fatalf("reconciled state = %#v err=%v, want anchored and acknowledged", current, err)
	}
}

type blobWakeFaultStore struct {
	storage.Store
	rejectWake   bool
	failedWakes  int
	rejectIntent bool
	intentWrites int
	afterWake    func()
}

func (s *blobWakeFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.rejectWake && strings.HasPrefix(key, "tasks/wake/") {
		s.failedWakes++
		return storage.Entry{}, storage.ErrUnavailable
	}
	if strings.HasPrefix(key, "solidification/blobs/intents/") {
		s.intentWrites++
		if s.rejectIntent {
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil && s.afterWake != nil && strings.HasPrefix(key, "tasks/wake/") {
		afterWake := s.afterWake
		s.afterWake = nil
		afterWake()
	}
	return entry, err
}
