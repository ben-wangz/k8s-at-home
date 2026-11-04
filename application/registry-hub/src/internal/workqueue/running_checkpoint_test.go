package workqueue

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestRunningCheckpointSurvivesTakeoverAndRejectsStaleFence(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "terminal_cleanup", "task/generation", "source", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Checkpoint(ctx, &job, map[string]int{"phase": 7}); err != nil {
		t.Fatal(err)
	}
	current, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil || current.State != "running" || current.Fence != job.Fence || string(current.Progress) != `{"phase":7}` {
		t.Fatalf("checkpoint released claim or lost progress: %+v %v", current, err)
	}
	current.LeaseUntil = time.Now().Add(-time.Second)
	if err := q.save(ctx, &current); err != nil {
		t.Fatal(err)
	}
	current, err = q.Claim(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Checkpoint(ctx, &job, map[string]int{"phase": 8}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale checkpoint accepted: %v", err)
	}
	if string(current.Progress) != `{"phase":7}` || current.Fence <= job.Fence {
		t.Fatalf("takeover lost checkpoint: %+v", current)
	}
}
