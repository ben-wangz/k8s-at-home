package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestBatchLogIsStableAndReservesCapacityBeforeWrite(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	event := Event{ID: "stable-event", OccurredAt: time.Now().UTC(), Component: "test", Operation: "batch", Stage: "done", Result: "done"}
	batch, _, reservation, err := buildEventBatch([]Event{event}, false)
	if err != nil {
		t.Fatal(err)
	}
	second := event
	second.ID = "ordinary-event"
	second.OccurredAt = event.OccurredAt.Add(time.Second)
	critical := second
	critical.ID = "critical-event"
	_, _, criticalReservation, err := buildEventBatch([]Event{critical}, true)
	if err != nil {
		t.Fatal(err)
	}
	policy := BatchPolicy{
		DailyBytes: reservation.Bytes + criticalReservation.Bytes, DailyBatches: 2,
		GlobalBytes: reservation.Bytes + criticalReservation.Bytes, GlobalBatches: 2,
		DailyCriticalBytes: criticalReservation.Bytes, DailyCriticalBatches: 1,
		GlobalCriticalBytes: criticalReservation.Bytes, GlobalCriticalBatches: 1,
	}
	log := BatchLog{Store: store, Policy: policy}
	written, err := log.Append(ctx, []Event{event}, false)
	if err != nil || written.ID != batch.ID {
		t.Fatalf("write batch: %+v %v", written, err)
	}
	if _, err := log.Append(ctx, []Event{event}, false); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if _, err := log.Append(ctx, []Event{second}, false); !errors.Is(err, ErrHistoryCapacity) {
		t.Fatalf("ordinary write = %v; want reserved capacity", err)
	}
	if _, err := log.Append(ctx, []Event{critical}, true); err != nil {
		t.Fatalf("critical write could not use reserved capacity: %v", err)
	}
	summary, err := log.Summary(ctx, EventBatchDate(event))
	if err != nil {
		t.Fatal(err)
	}
	if summary.DailyBatches != 2 || summary.GlobalBatches != 2 || summary.RejectedBatches != 1 || summary.DailyBytes != reservation.Bytes+criticalReservation.Bytes {
		t.Fatalf("budget does not count stable reservations: %+v", summary)
	}
	entries, _, err := store.List(ctx, BatchPrefix, "", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("stored batches = %d, error = %v", len(entries), err)
	}
}

func TestObservationBatchPolicyUsesConfiguredStartingCeilings(t *testing.T) {
	policy := ObservationBatchPolicy()
	if policy.DailyBytes != 4<<20 || policy.DailyBatches != 128 || policy.GlobalBytes != 32<<20 || policy.GlobalBatches != 1024 ||
		policy.DailyCriticalBytes != 1<<20 || policy.DailyCriticalBatches != 32 || policy.GlobalCriticalBytes != 8<<20 || policy.GlobalCriticalBatches != 256 {
		t.Fatalf("observation batch policy = %+v", policy)
	}
}

func TestBatchLogUsesConfiguredRetention(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := Event{ID: "expired-configured-batch", OccurredAt: time.Now().UTC().Add(-2 * time.Second)}
	if _, err := (BatchLog{Store: store, Retention: time.Second}).Append(context.Background(), []Event{event}, false); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("batch older than configured retention = %v", err)
	}
}

func TestBuildEventBatchRejectsContentMismatchAndPartitionCrossing(t *testing.T) {
	first := Event{ID: "same", OccurredAt: time.Now().UTC(), Component: "test"}
	conflicting := first
	conflicting.Result = "failed"
	if _, _, _, err := buildEventBatch([]Event{first, conflicting}, false); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("conflicting duplicate ID = %v", err)
	}
	second := first
	second.ID = "next-day"
	second.OccurredAt = time.Date(first.OccurredAt.UTC().Year(), first.OccurredAt.UTC().Month(), first.OccurredAt.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
	if _, _, _, err := buildEventBatch([]Event{first, second}, false); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("cross-partition batch = %v", err)
	}
}

func TestBatchLogLargeEventSetStaysWithinConfiguredRecordLimit(t *testing.T) {
	events := make([]Event, BatchMaxEvents)
	now := time.Now().UTC()
	for index := range events {
		events[index] = Event{ID: fmt.Sprintf("event-%03d", index), OccurredAt: now, Component: "test", Operation: "batch"}
	}
	batch, data, _, err := buildEventBatch(events, false)
	if err != nil || len(batch.Events) != BatchMaxEvents || len(data) > BatchMaxBytes {
		t.Fatalf("bounded batch: events=%d bytes=%d err=%v", len(batch.Events), len(data), err)
	}
}
