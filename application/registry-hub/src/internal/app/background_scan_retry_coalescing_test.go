package app

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

func TestSourceSweepLeavesPendingVersionToRetryDispatcher(t *testing.T) {
	ctx := context.Background()
	s := newStagedAcceptanceFixture(t, 1<<20).service
	kind, prefix := "receipt_cleanup_preview", "test/receipts/"
	first, err := putSourceSweepFixture(ctx, s.store, prefix+"first.json", []byte("receipt"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := putSourceSweepFixture(ctx, s.store, prefix+"second.json", []byte("receipt"))
	if err != nil {
		t.Fatal(err)
	}
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "background/source/"+kind, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)
	calls := map[string]int{}
	step := func(_ context.Context, entry storage.Entry, _ *sourceSweepCheckpoint) error {
		calls[entry.Key]++
		return nil
	}
	if err := s.enqueueSourceSweepRetry(ctx, kind, first, storage.ErrTimeout, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueueSourceSweepRetry(ctx, kind, second, storage.ErrTimeout, 1, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	other, err := putSourceSweepFixture(ctx, s.store, prefix+"third.json", []byte("receipt"))
	if err != nil {
		t.Fatal(err)
	}
	p := sourceSweepCheckpoint{Round: 2, Backoff: map[string]entityBackoff{}}
	s.sourceSweepBatch(ctx, kind, prefix, &p, step)
	if calls[first.Key] != 0 || calls[second.Key] != 0 || calls[other.Key] != 1 || p.Cursor.Entities != 3 {
		t.Fatalf("normal scan calls=%v cursor=%+v", calls, p.Cursor)
	}
	processed, err := s.dispatchDueSourceSweepRetry(ctx, holder, kind, &p, step)
	if err != nil || !processed || calls[first.Key] != 0 || calls[second.Key] != 1 {
		t.Fatalf("retry processed=%t calls=%v err=%v", processed, calls, err)
	}
	if pending, err := s.sourceSweepRetryPending(ctx, kind, second); err != nil || pending {
		t.Fatalf("completed retry remains pending=%t err=%v", pending, err)
	}
}
