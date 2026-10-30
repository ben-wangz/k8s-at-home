package workqueue

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/storage"
)

type retryAcceptanceFailure struct {
	storage.Store
	fail bool
}

func (s *retryAcceptanceFailure) Put(ctx context.Context, key string, body io.Reader, size int64, opts storage.PutOptions) (storage.Entry, error) {
	if s.fail && strings.HasPrefix(key, Prefix+"retry-requests/") && opts.IfMatch != "" {
		s.fail = false
		return storage.Entry{}, storage.ErrTimeout
	}
	return s.Store.Put(ctx, key, body, size, opts)
}

func TestConditionalRetryRecoversAcceptanceWriteFailure(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "inventory", "retry", "source", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Finish(ctx, &job, nil, false, ErrSourceMissing); err != nil {
		t.Fatal(err)
	}
	request := RetryRequest{ExpectedRevision: Revision(job), SourceRevision: SourceRevision(job), IdempotencyKey: "retry-one"}
	fault := &retryAcceptanceFailure{Store: q.Store, fail: true}
	q.Store = fault
	if _, err = q.AcceptRetry(ctx, "operator", job.Kind, job.ID, request); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("expected acceptance write failure: %v", err)
	}
	current, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "pending" {
		t.Fatalf("retry not durably accepted: %+v", current)
	}
	current, err = q.Claim(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Finish(ctx, &current, nil, false, ErrSourceMissing); err != nil {
		t.Fatal(err)
	}
	second := RetryRequest{ExpectedRevision: Revision(current), SourceRevision: SourceRevision(current), IdempotencyKey: "retry-two"}
	if _, err = q.AcceptRetry(ctx, "operator", current.Kind, current.ID, second); err != nil {
		t.Fatal(err)
	}
	a, err := q.AcceptRetry(ctx, "operator", job.Kind, job.ID, request)
	if err != nil || a.AcceptedAt.IsZero() {
		t.Fatalf("original acceptance lost after later retry: %+v %v", a, err)
	}
	b, err := q.AcceptRetry(ctx, "operator", job.Kind, job.ID, request)
	if err != nil || !a.AcceptedAt.Equal(b.AcceptedAt) {
		t.Fatalf("duplicate changed acceptance: %+v %v", b, err)
	}
	request.IdempotencyKey = "stale-key"
	if _, err = q.AcceptRetry(ctx, "operator", job.Kind, job.ID, request); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("stale revision accepted: %v", err)
	}
}
