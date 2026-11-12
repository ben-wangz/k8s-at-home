package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func TestRecoveryPauseIsOnlyAWaitWithoutOtherFailures(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1024)
	for _, err := range []error{errRecoveryPaused, ErrRecoveryBarrier, errors.Join(errRecoveryPaused, ErrRecoveryBarrier)} {
		if _, ok := workqueue.Dependency(f.service.dependencyResult(err)); !ok {
			t.Fatalf("pause not classified: %v", err)
		}
	}
	for _, err := range []error{storage.ErrTimeout, storage.ErrUnknownCommit, storage.ErrUnavailable, storage.ErrLeaseLost} {
		if _, ok := workqueue.Dependency(f.service.dependencyResult(errors.Join(errRecoveryPaused, err))); ok {
			t.Fatalf("real failure hidden: %v", err)
		}
	}
	p := sourceSweepCheckpoint{}
	if !f.service.sourceDependencyWait(context.Background(), "activation", &p, errRecoveryPaused) || p.ScanRetries != 0 || p.Status.ErrorAt.IsZero() == false || !p.Status.ProgressAt.IsZero() || p.Status.WaitReason != "protection_recovery" {
		t.Fatalf("scanner wait: %+v", p)
	}
	view := diagnosticJob(workqueue.Job{WaitReason: "protection_recovery", State: "pending", ResponsibleWorkKind: "protection_recovery", ResponsibleWorkID: "generation"})
	if view.State != "waiting_dependency" || view.ResponsibleWorkURL != "/api/v1/workers/protection_recovery" || view.ErrorBlocking {
		t.Fatalf("wait diagnostic: %+v", view)
	}
}

func TestRecoveryQueueAdmissionAndCompletionRecheck(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1024)
	q := workqueue.Queue{Store: f.service.store}
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "wake_root", "recovery", "root", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.service.recoveryStarting.Store(true)
	for n := 0; n < 3; n++ {
		waiting, err := f.service.prepareQueueDependency(ctx, q, &job)
		if err != nil || !waiting || job.Fence != 0 || job.RetryCount != 0 || job.WaitReason != "protection_recovery" {
			t.Fatalf("admitted frozen work: %+v %v", job, err)
		}
	}
	f.service.recoveryStarting.Store(false)
	job, err = q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := f.service.prepareQueueDependency(ctx, q, &job)
	if err != nil || waiting || job.WaitReason != "" || !job.NextAttemptAt.IsZero() {
		t.Fatalf("completion not replayable: %+v %v", job, err)
	}
	if _, err = q.Claim(ctx, job); err != nil {
		t.Fatal(err)
	}
}
