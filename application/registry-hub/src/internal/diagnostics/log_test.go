package diagnostics

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestHistoryIsIdempotentBoundedAndExpires(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	log := Log{Store: store}
	event := Event{ID: ID("event"), OccurredAt: time.Now().UTC(), Stage: "execution", Result: "failed", ErrorClass: "timeout"}
	if err := log.Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	budgetKey := Prefix + "event-budgets/" + event.OccurredAt.Format("20060102") + ".json"
	before, _, err := inventory.Read[HistoryBudget](ctx, store, budgetKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	after, _, _ := inventory.Read[HistoryBudget](ctx, store, budgetKey)
	if before.ReservedBytes != after.ReservedBytes {
		t.Fatal("duplicate consumed history budget")
	}
	if err := inventory.Update(ctx, store, budgetKey, func(value *HistoryBudget) error { value.ReservedBytes = DailyBytesLimit; return nil }); err != nil {
		t.Fatal(err)
	}
	event.ID = ID("rejected")
	if !errors.Is(log.Append(ctx, event), ErrHistoryCapacity) {
		t.Fatal("daily budget not enforced")
	}
	if _, err := store.Head(ctx, EventKey(event)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("rejected event persisted")
	}
	event.OccurredAt = time.Now().Add(-Retention - time.Hour)
	if err := log.Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Head(ctx, EventKey(event)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("expired source history resurrected")
	}
	var outbox []Event
	for n := 0; n < OutboxLimit+5; n++ {
		Record(&outbox, Event{Stage: "attempt", Result: "failed", Attempt: uint64(n), ErrorClass: "timeout"})
	}
	Record(&outbox, Event{Stage: "attempt", Result: "done", Attempt: OutboxLimit + 5})
	if len(outbox) != OutboxLimit || outbox[len(outbox)-1].RecoveredAt.IsZero() {
		t.Fatal("outbox retention or recovery lost")
	}
}
