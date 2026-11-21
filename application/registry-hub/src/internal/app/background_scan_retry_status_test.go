package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

func TestUpdateSourceRetryStatusPreservesSnapshotOnReadFailure(t *testing.T) {
	for _, failure := range []string{"list", "get"} {
		t.Run(failure, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			entry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt"))
			if err != nil {
				t.Fatal(err)
			}
			dueAt := time.Now().UTC().Add(time.Hour)
			if err := f.service.enqueueSourceSweepRetry(ctx, "history_receipts", entry, storage.ErrUnavailable, 1, dueAt); err != nil {
				t.Fatal(err)
			}
			faults := &retryStatusFailureStore{Store: f.service.store}
			if failure == "list" {
				faults.failDueStatusList.Store(true)
			} else {
				faults.failDueStatusGet.Store(true)
			}
			f.service.store = faults
			oldDue, oldNext := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(-time.Second)
			checkpoint := sourceSweepCheckpoint{Status: backgroundScanStatus{
				PendingRetries: 9, OldestDueRetryAt: oldDue, NextRetryAt: oldNext,
			}}
			if err := f.service.updateSourceRetryStatus(ctx, "history_receipts", &checkpoint); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatalf("status refresh error = %v, want unavailable", err)
			}
			assertRetryStatus(t, checkpoint.Status, 9, oldDue, oldNext)

			faults.failDueStatusList.Store(false)
			faults.failDueStatusGet.Store(false)
			if err := f.service.updateSourceRetryStatus(ctx, "history_receipts", &checkpoint); err != nil {
				t.Fatal(err)
			}
			assertRetryStatus(t, checkpoint.Status, 1, time.Time{}, dueAt)
		})
	}
}

func TestRetryStatusFailureDoesNotAccelerateSourceScan(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	kind, prefix, partition := "diagnostic_retention_events", "diagnostics/v1/events/", "20260101"
	if _, err := putSourceSweepFixture(ctx, f.service.store, prefix+partition+"/one.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, f.service.store, sourceSweepKey(kind), func(value *sourceSweepCheckpoint) error {
		value.EventPartitionDate, value.EventRoundEndDate = partition, partition
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	faults := &retryStatusFailureStore{Store: f.service.store}
	faults.failDueStatusList.Store(true)
	f.service.store = faults
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, "background/source/"+kind, "retry-status-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)

	bounded, cancel := context.WithTimeout(ctx, 7500*time.Millisecond)
	defer cancel()
	scans := 0
	f.service.runSourceSweepStateLeader(bounded, holder, kind, prefix, func(context.Context, storage.Entry, *sourceSweepCheckpoint) error {
		scans++
		return nil
	}, nil)
	if scans != 1 {
		t.Fatalf("source scans during the normal 15s interval = %d, want 1", scans)
	}
	if faults.dueStatusListCalls.Load() == 0 {
		t.Fatal("test did not trigger a retry status index read")
	}
	checkpoint, _, err := inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey(kind))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.RoundFailed || checkpoint.ScanRetries != 0 || !checkpoint.Status.Complete {
		t.Fatalf("status read failure altered scan result: failed=%t retries=%d status=%+v", checkpoint.RoundFailed, checkpoint.ScanRetries, checkpoint.Status)
	}
	if !checkpoint.NextRound.After(time.Now().Add(5 * time.Second)) {
		t.Fatalf("next scan scheduled too soon after status failure: %s", checkpoint.NextRound)
	}
}

func TestCompletedRetrySurvivesStatusReadFailureAndRefreshesLater(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	kind := "history_receipts"
	dueEntry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/due.json", []byte("due"))
	if err != nil {
		t.Fatal(err)
	}
	futureEntry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/future.json", []byte("future"))
	if err != nil {
		t.Fatal(err)
	}
	dueAt, futureAt := time.Now().UTC().Add(-time.Second), time.Now().UTC().Add(time.Hour)
	if err := f.service.enqueueSourceSweepRetry(ctx, kind, dueEntry, storage.ErrUnavailable, 1, dueAt); err != nil {
		t.Fatal(err)
	}
	if err := f.service.enqueueSourceSweepRetry(ctx, kind, futureEntry, storage.ErrUnavailable, 1, futureAt); err != nil {
		t.Fatal(err)
	}
	faults := &retryStatusFailureStore{Store: f.service.store}
	faults.failDueStatusList.Store(true)
	f.service.store = faults
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, "background/source/"+kind, "retry-status-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)

	checkpoint := &sourceSweepCheckpoint{Round: 1, Status: backgroundScanStatus{
		PendingRetries: 2, OldestDueRetryAt: dueAt, NextRetryAt: dueAt,
	}}
	dispatchState := sourceRetryDispatchState{backoff: time.Second}
	calls := 0
	processed, err := f.service.dispatchScheduledSourceRetry(ctx, holder, kind, checkpoint,
		func(context.Context, storage.Entry, *sourceSweepCheckpoint) error { calls++; return nil },
		dueAt, false, &dispatchState)
	if err != nil || !processed || calls != 1 {
		t.Fatalf("completed retry processed=%t calls=%d err=%v", processed, calls, err)
	}
	if err := manager.Validate(ctx, holder); err != nil {
		t.Fatalf("leader lease was lost after retry checkpoint: %v", err)
	}
	if checkpoint.Status.PendingRetries != 2 {
		t.Fatalf("failed refresh changed cached pending retry count to %d", checkpoint.Status.PendingRetries)
	}
	stored, _, err := inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey(kind))
	if err != nil || stored.Status.CheckpointAt.IsZero() {
		t.Fatalf("completed retry checkpoint was not saved: status=%+v err=%v", stored.Status, err)
	}

	faults.failDueStatusList.Store(false)
	refreshState := sourceRetryStatusRefreshState{backoff: sourceRetryStatusRefreshInterval}
	if err := f.service.refreshSourceRetryStatusIfDue(ctx, holder, kind, checkpoint, &refreshState); err != nil {
		t.Fatal(err)
	}
	assertRetryStatus(t, checkpoint.Status, 1, time.Time{}, futureAt)
	stored, _, err = inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey(kind))
	if err != nil {
		t.Fatal(err)
	}
	assertRetryStatus(t, stored.Status, 1, time.Time{}, futureAt)
}

func TestCompletedRetryStillYieldsOnCheckpointWriteFailure(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	kind := "history_receipts"
	entry, err := putSourceSweepFixture(ctx, f.service.store, "test/receipts/one.json", []byte("receipt"))
	if err != nil {
		t.Fatal(err)
	}
	dueAt := time.Now().UTC().Add(-time.Second)
	if err := f.service.enqueueSourceSweepRetry(ctx, kind, entry, storage.ErrUnavailable, 1, dueAt); err != nil {
		t.Fatal(err)
	}
	faults := &retryStatusFailureStore{Store: f.service.store}
	faults.failCheckpointPut.Store(true)
	f.service.store = faults
	manager := lease.NewManager(f.service.store)
	holder, err := manager.Acquire(ctx, "background/source/"+kind, "retry-status-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)

	processed, err := f.service.dispatchScheduledSourceRetry(ctx, holder, kind, &sourceSweepCheckpoint{Round: 1},
		func(context.Context, storage.Entry, *sourceSweepCheckpoint) error { return nil },
		dueAt, false, &sourceRetryDispatchState{backoff: time.Second})
	if processed || !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("checkpoint failure returned processed=%t err=%v, want unavailable", processed, err)
	}
}

func assertRetryStatus(t *testing.T, status backgroundScanStatus, pending int64, oldest, next time.Time) {
	t.Helper()
	if status.PendingRetries != pending || !status.OldestDueRetryAt.Equal(oldest) || !status.NextRetryAt.Equal(next) {
		t.Fatalf("retry status = pending:%d oldest:%s next:%s, want pending:%d oldest:%s next:%s",
			status.PendingRetries, status.OldestDueRetryAt, status.NextRetryAt, pending, oldest, next)
	}
}

type retryStatusFailureStore struct {
	storage.Store
	failDueStatusList  atomic.Bool
	failDueStatusGet   atomic.Bool
	failCheckpointPut  atomic.Bool
	dueStatusListCalls atomic.Int32
}

func (s *retryStatusFailureStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.failDueStatusGet.Load() && strings.HasPrefix(key, sourceRetryDuePrefix) {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *retryStatusFailureStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if strings.HasPrefix(prefix, sourceRetryDuePrefix) && limit == 1000 {
		s.dueStatusListCalls.Add(1)
		if s.failDueStatusList.Load() {
			return nil, "", storage.ErrUnavailable
		}
	}
	return s.Store.List(ctx, prefix, token, limit)
}

func (s *retryStatusFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.failCheckpointPut.Load() && key == sourceSweepKey("history_receipts") {
		return storage.Entry{}, storage.ErrUnavailable
	}
	return s.Store.Put(ctx, key, body, size, options)
}
