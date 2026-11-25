package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestDiagnosticBatchBackoffDefersSameDateAndLetsOtherDatesFlush(t *testing.T) {
	for _, fault := range []string{"lease read", "scan read", "locked read", "flush write"} {
		t.Run(fault, func(t *testing.T) {
			testDiagnosticBatchBackoff(t, fault)
		})
	}
}

func testDiagnosticBatchBackoff(t *testing.T, fault string) {
	t.Helper()
	ctx := context.Background()
	base := &diagnosticFlushFaultStore{Store: newFilesystemStore(t)}
	store := &diagnosticBatchReadFaultStore{Store: base}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-24*time.Hour + time.Hour)
	admit := func(id string, at time.Time) error {
		event := diagnostics.Event{ID: id, OccurredAt: at, Result: "pending"}
		if err := s.collectDiagnosticOutboxVersion(ctx, workqueue.Key("notify", id), "v1", []diagnostics.Event{event}); err != nil {
			return err
		}
		return inventory.Update(ctx, s.store, diagnosticPendingBatchKey(at.Format("20060102")), func(partition *diagnosticPendingPartition) error {
			partition.Batch.OrdinaryDeadline = time.Now().Add(-time.Minute)
			return nil
		})
	}
	if err := admit("old", day); err != nil {
		t.Fatal(err)
	}
	date := day.Format("20060102")
	switch fault {
	case "lease read":
		store.key, store.failAt = "leases/"+diagnosticPartitionLeaseKey(date), 1
	case "scan read":
		store.key, store.failAt = diagnosticPendingBatchKey(date), 1
	case "locked read":
		store.key, store.failAt = diagnosticPendingBatchKey(date), 2
	case "flush write":
		base.failIntentWrites = 1
	}
	if err := s.flushDueDiagnosticBatches(ctx, time.Now().UTC()); !errors.Is(err, storage.ErrUnavailable) && !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("initial flush did not reach the injected failure: %v", err)
	}
	if due := s.diagnosticBatchBackoff.dueAt(date); !due.After(time.Now()) {
		t.Fatalf("failed flush did not schedule a retry: %v", due)
	}
	blockedUntil := time.Now().Add(time.Hour)
	s.diagnosticBatchBackoff.mu.Lock()
	retry := s.diagnosticBatchBackoff.retries[date]
	retry.dueAt = blockedUntil
	s.diagnosticBatchBackoff.retries[date] = retry
	s.diagnosticBatchBackoff.mu.Unlock()
	var deferred diagnosticPartitionDeferred
	if err := admit("same-day", day); !errors.As(err, &deferred) {
		t.Fatalf("same-day admission during backoff: %v", err)
	}
	other := day.Add(24 * time.Hour)
	if err := admit("other-day", other); err != nil {
		t.Fatalf("other date admission: %v", err)
	}
	next, batchErr, scanErr := s.scanDiagnosticPendingBatches(ctx, time.Now().UTC())
	if batchErr != nil || scanErr != nil || !next.Equal(blockedUntil) {
		t.Fatalf("next flush=%v batch err=%v scan err=%v", next, batchErr, scanErr)
	}
	if _, err := s.store.Head(ctx, diagnosticPendingBatchKey(date)); err != nil {
		t.Fatalf("failed batch changed during backoff: %v", err)
	}
	if _, err := s.store.Head(ctx, diagnosticPendingBatchKey(other.Format("20060102"))); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("other date did not flush: %v", err)
	}
	s.diagnosticBatchBackoff.clear(date)
	if err := s.flushDueDiagnosticBatches(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("scheduled retry after backoff: %v", err)
	}
	if _, err := s.store.Head(ctx, diagnosticPendingBatchKey(date)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old batch did not settle: %v", err)
	}
	if err := admit("same-day", day); err != nil {
		t.Fatalf("deferred event could not be admitted after the batch settled: %v", err)
	}
}

type diagnosticBatchReadFaultStore struct {
	storage.Store
	key    string
	failAt int
	reads  int
}

func (s *diagnosticBatchReadFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == s.key {
		s.reads++
		if s.reads == s.failAt {
			return storage.Object{}, storage.ErrTimeout
		}
	}
	return s.Store.Get(ctx, key)
}
