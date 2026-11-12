package workqueue

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestDependencyWaitPreservesProgressAndFailureHistory(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "wake_root", "dependency", "root", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job.RetryCount, job.ErrorClass, job.ErrorAt = 3, "timeout", time.Now().UTC().Add(-time.Hour)
	if err := q.save(ctx, &job); err != nil {
		t.Fatal(err)
	}
	before := job.ProgressAt
	for n := 0; n < 3; n++ {
		job, err = q.Claim(ctx, job)
		if err != nil {
			t.Fatal(err)
		}
		wait := &DependencyWait{Reason: "protection_recovery", Generation: "generation", WorkKind: "protection_recovery", WorkID: "generation"}
		if err = q.Finish(ctx, &job, map[string]int{"offset": 7}, false, wait); err != nil {
			t.Fatal(err)
		}
		if job.State != "pending" || job.WaitReason != wait.Reason || job.DependencyGeneration != wait.Generation || job.RetryCount != 3 || job.ErrorClass != "timeout" || !job.ProgressAt.Equal(before) {
			t.Fatalf("dependency changed failure/progress evidence: %+v", job)
		}
		q = Queue{Store: q.Store}
		job, err = q.Read(ctx, job.Kind, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = q.ResumeDependency(ctx, &job); err != nil {
			t.Fatal(err)
		}
		if !job.NextAttemptAt.IsZero() || job.RunnableAt.IsZero() {
			t.Fatal("dependency retained backoff")
		}
	}
	for _, event := range job.DiagnosticOutbox {
		if event.Result == "waiting_dependency" && (event.ErrorClass != "" || event.Retryable) {
			t.Fatalf("wait reported failure: %+v", event)
		}
	}
}

func TestDependencyJoinedFailuresRemainFailures(t *testing.T) {
	w := &DependencyWait{Reason: "protection_recovery", Generation: "one"}
	for _, cause := range []error{storage.ErrTimeout, storage.ErrUnavailable, storage.ErrUnknownCommit, storage.ErrLeaseLost, context.DeadlineExceeded} {
		err := fmt.Errorf("operation: %w", errors.Join(w, cause))
		if _, ok := Dependency(err); ok {
			t.Fatalf("failure hidden: %v", err)
		}
		q := queueFixture(t)
		job, _ := q.Enqueue(context.Background(), "wake_root", cause.Error(), "root", time.Now().UTC())
		job, _ = q.Claim(context.Background(), job)
		if finishErr := q.Finish(context.Background(), &job, nil, false, err); finishErr != nil {
			t.Fatal(finishErr)
		}
		if job.State != "retry_wait" || job.RetryCount != 1 {
			t.Fatalf("failure not retried: %+v", job)
		}
	}
}

func TestDependencyWaitTakesOverExpiredClaimWithoutFailure(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, _ := q.Enqueue(ctx, "wake_root", "takeover", "root", time.Now().UTC())
	job, _ = q.Claim(ctx, job)
	job.LeaseUntil = time.Now().Add(-time.Second)
	if err := q.save(ctx, &job); err != nil {
		t.Fatal(err)
	}
	old := job
	if err := q.Wait(ctx, &job, &DependencyWait{Reason: "protection_recovery", Generation: "restarted"}); err != nil {
		t.Fatal(err)
	}
	if job.State != "pending" || job.Fence <= old.Fence || job.RetryCount != 0 || !job.ProgressAt.Equal(old.ProgressAt) {
		t.Fatalf("takeover manufactured failure or progress: %+v", job)
	}
	if err := q.Finish(ctx, &old, nil, true, nil); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("expired execution acknowledged after wait takeover: %v", err)
	}
}
