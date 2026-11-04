package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestTerminalSharedDemandSnapshotWaitsForAdmissionCommit(t *testing.T) {
	f, faults, root := terminalFixture(t, 1)
	ctx := context.Background()
	demands, _, err := f.service.records.ListRootBlobDemandRecords(ctx, root.ID, "", 1)
	if err != nil || len(demands) != 1 {
		t.Fatal(err)
	}
	demand := demands[0]
	if err := f.service.records.RemoveBlobDemand(ctx, demand); err != nil {
		t.Fatal(err)
	}
	other := newDemandRootTask(t, ctx, f.service.tasks, "in-progress-admission")
	id, session := mustGateTestID(t), mustGateTestID(t)
	entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	faults.putHook = func(key string, _ []byte) error {
		if strings.HasPrefix(key, "solidification/blobs/owners/"+demand.IntentID+"/") {
			close(entered)
			<-resume
		}
		return nil
	}
	go func() {
		finished <- f.service.records.LinkBlobDemandScope(ctx, demand.IntentID, id, other.ID, "new-scope", session, 64)
	}()
	<-entered
	r := &protectionRecovery{service: f.service, checkpoint: protectionRecoveryCheckpoint{Step: "demand_revision", Demand: &demand}}
	p := terminalCleanupProgress{}
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	err = f.service.terminalSharedDemandStep(bounded, r, &p)
	cancel()
	close(resume)
	if linkErr := <-finished; linkErr != nil {
		t.Fatal(linkErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) || r.checkpoint.Step != "demand_revision" {
		t.Fatalf("snapshot observed an uncommitted admission: step=%s error=%v", r.checkpoint.Step, err)
	}
	for unit := 0; unit < 10 && r.checkpoint.Step != "demand_delete"; unit++ {
		if err := f.service.terminalSharedDemandStep(ctx, r, &p); err != nil {
			t.Fatal(err)
		}
	}
	if r.checkpoint.Step != "demand_delete" || !r.checkpoint.ActiveDemand {
		t.Fatalf("committed admission was missed: %+v", r.checkpoint)
	}
}

func TestTerminalSharedDemandRevalidatesAdmissionAfterScan(t *testing.T) {
	f, _, root := terminalFixture(t, 1)
	ctx := context.Background()
	demands, _, err := f.service.records.ListRootBlobDemandRecords(ctx, root.ID, "", 1)
	if err != nil || len(demands) != 1 {
		t.Fatal(err)
	}
	demand := demands[0]
	if err := f.service.records.RemoveBlobDemand(ctx, demand); err != nil {
		t.Fatal(err)
	}
	r := &protectionRecovery{service: f.service, checkpoint: protectionRecoveryCheckpoint{Step: "demand_revision", Demand: &demand}}
	p := terminalCleanupProgress{}
	for unit := 0; unit < 10 && r.checkpoint.Step != "demand_cancel"; unit++ {
		if err := f.service.terminalSharedDemandStep(ctx, r, &p); err != nil {
			t.Fatal(err)
		}
	}
	if r.checkpoint.Step != "demand_cancel" {
		t.Fatal("shared scan did not reach the cancellation fence")
	}
	other := newDemandRootTask(t, ctx, f.service.tasks, "concurrent-admission")
	linkRootBlobDemand(t, ctx, f.service.records, demand.IntentID, other.ID, mustGateTestID(t), "new-scope", mustGateTestID(t))
	if err := f.service.terminalSharedDemandStep(ctx, r, &p); err != nil || r.checkpoint.Step != "demand_revision" {
		t.Fatalf("new demand was not fenced by its revision: step=%s error=%v", r.checkpoint.Step, err)
	}
	for unit := 0; unit < 10 && r.checkpoint.Step != "demand_delete"; unit++ {
		if err := f.service.terminalSharedDemandStep(ctx, r, &p); err != nil {
			t.Fatal(err)
		}
	}
	blob, err := f.service.tasks.Get(ctx, demand.IntentID)
	if err != nil || blob.CancelRequested || r.checkpoint.Step != "demand_delete" || !r.checkpoint.ActiveDemand {
		t.Fatalf("concurrent demand lost its blob task: %+v checkpoint=%+v error=%v", blob, r.checkpoint, err)
	}
}

func TestTerminalMutationRejectsStaleWorkFenceBeforeDelete(t *testing.T) {
	f, _, root := terminalFixture(t, 1)
	ctx := context.Background()
	root, err := f.service.tasks.Cancel(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := workqueue.Queue{Store: f.service.store}
	job, err := q.Claim(ctx, terminalJob(t, f.service, root))
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(f.service.store)
	held, err := manager.Acquire(ctx, "tasks/"+root.ID, "old-terminal", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, held)
	var work terminalCleanupWork
	if err := json.Unmarshal(job.Payload, &work); err != nil {
		t.Fatal(err)
	}
	progress := terminalCleanupProgress{}
	scope := &terminalWriteScope{service: f.service, job: &job, work: work, progress: &progress, parent: ctx, manager: manager, lease: held, epoch: f.service.gate.State().Epoch}
	if err := inventory.Update(ctx, f.service.store, workqueue.Key(job.Kind, job.ID), func(current *workqueue.Job) error {
		current.LeaseUntil = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	current, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, current); err != nil {
		t.Fatal(err)
	}
	demands, _, err := f.service.records.ListRootBlobDemandRecords(ctx, root.ID, "", 1)
	if err != nil || len(demands) != 1 {
		t.Fatal(err)
	}
	stale := context.WithValue(ctx, terminalWriteScopeKey{}, scope)
	err = f.service.records.RemoveBlobDemand(stale, demands[0])
	if !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("old worker issued a deletion: %v", err)
	}
	remaining, _, err := f.service.records.ListBlobDemandRecords(ctx, demands[0].IntentID, "", 1)
	if err != nil || len(remaining) != 1 || f.service.gate.State().RecoveryBarrier {
		t.Fatalf("stale worker changed source or gate: %+v %v", remaining, err)
	}
}
