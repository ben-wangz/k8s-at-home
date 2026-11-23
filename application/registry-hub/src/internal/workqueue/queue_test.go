package workqueue

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func queueFixture(t *testing.T) Queue {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return Queue{Store: store}
}

func TestExecutionReceiptPreservesOriginalFactAcrossReplay(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	original := time.Now().UTC().Add(-time.Hour)
	job, err := q.Enqueue(ctx, "execution", "source", map[string]string{"digest": "one"}, original)
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &job, map[string]int{"phase": 3}, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Store.Head(ctx, Key(job.Kind, job.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("active job remains: %v", err)
	}
	replayed, err := q.Enqueue(ctx, "execution", "source", map[string]string{"digest": "one"}, time.Now().UTC())
	if err != nil || replayed.State != "done" || !replayed.CreatedAt.Equal(original) {
		t.Fatalf("replay=%+v err=%v", replayed, err)
	}
	if _, err := q.Enqueue(ctx, "execution", "source", map[string]string{"digest": "two"}, original); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestCompletionMarkerPreservesIdempotencyAfterReceiptRemoval(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	original := time.Now().UTC().Add(-time.Hour)
	payload := map[string]string{"digest": "one"}
	job, err := q.Enqueue(ctx, "notify", "marker", payload, original)
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &job, map[string]int{"complete": 1}, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := q.PersistCompletionMarker(ctx, job); err != nil {
		t.Fatalf("persist completion marker: %v", err)
	}
	if _, _, err := inventory.Read[CompletionMarker](ctx, q.Store, CompletionMarkerKey(job.Kind, job.ID)); err != nil {
		t.Fatalf("missing completion marker: %v", err)
	}
	receipt, version, err := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID))
	if err != nil || receipt.State != "done" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if err := q.Store.DeleteIfMatch(ctx, ReceiptKey(job.Kind, job.ID), version); err != nil {
		t.Fatal(err)
	}
	replay, err := q.Enqueue(ctx, "notify", "marker", payload, time.Now().UTC())
	if err != nil || replay.State != "done" || replay.ID != job.ID || replay.CompletedAt.IsZero() {
		t.Fatalf("completed replay=%+v err=%v", replay, err)
	}
	if _, err := q.Enqueue(ctx, "notify", "marker", map[string]string{"digest": "two"}, original); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("conflicting payload accepted after receipt removal: %v", err)
	}
}

func TestClaimRejectsAndRetiresDuplicateCoveredByCompletionMarker(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "notify", "late-duplicate", "payload", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	terminal := job
	terminal.State, terminal.CompletedAt, terminal.FinalSummary = "done", time.Now().UTC(), json.RawMessage(`{"done":true}`)
	if err := q.writeCompletionMarker(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Claim(ctx, job); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("late duplicate claimed: %v", err)
	}
	if _, err := q.Store.Head(ctx, Key(job.Kind, job.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("duplicate active job was not conditionally retired: %v", err)
	}
}

func TestExpiredWorkerCannotPublishOrAcknowledgeAfterTakeover(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "inventory", "source", "payload", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	old, err := q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	expired := old
	expired.LeaseUntil = time.Now().Add(-time.Second)
	if err := q.save(ctx, &expired); err != nil {
		t.Fatal(err)
	}
	current, err := q.Claim(ctx, expired)
	if err != nil {
		t.Fatal(err)
	}
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return q.Validate(ctx, old) })
	if err := inventory.Update(guarded, q.Store, "output.json", func(value *bool) error { *value = true; return nil }); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale output=%v", err)
	}
	if err := q.Finish(ctx, &old, nil, true, nil); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale acknowledgement=%v", err)
	}
	if err := q.Finish(ctx, &current, map[string]int{"phase": 1}, true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownCommitsAndPermanentFailureCanBeRecovered(t *testing.T) {
	q := queueFixture(t)
	fault := &unknownCommitStore{Store: q.Store}
	q.Store = fault
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "inventory", "source", "payload", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &job, map[string]int{"phase": 2}, false, ErrSourceMissing); err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || job.ErrorClass != "source_missing" || job.ErrorPhase != 2 {
		t.Fatalf("failure=%+v", job)
	}
	if err := q.Retry(ctx, job.Kind, job.ID); err != nil {
		t.Fatal(err)
	}
	job, err = q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &job, map[string]int{"phase": 3}, true, nil); err != nil {
		t.Fatal(err)
	}
	receipt, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil || receipt.State != "done" || receipt.ErrorClass != "source_missing" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
}

type unknownCommitStore struct{ storage.Store }

func (s *unknownCommitStore) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, opts)
	if err == nil && strings.HasPrefix(key, Prefix) {
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, err
}
