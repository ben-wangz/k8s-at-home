package app

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestDiagnosticSourceWakeArchivesDurableOutbox(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	job := workqueue.Job{
		SchemaVersion: 1, ID: workqueue.ID("notify", "source-wake"), Kind: "notify", State: "pending",
		DiagnosticOutbox: []diagnostics.Event{{ID: diagnostics.ID("event", "source-wake"), OccurredAt: time.Now().UTC(), Component: "background", Operation: "notify", Result: "failed"}},
	}
	key := workqueue.Key(job.Kind, job.ID)
	if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := f.service.collectDiagnosticSourceOutbox(ctx, key, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	stored, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(key)+".json")
	if err != nil || !diagnosticOutboxSettled(key, sourceEntryVersionFromStore(t, f.service.store, key), job.DiagnosticOutbox, stored) {
		t.Fatalf("outbox receipt = %+v, error = %v", stored, err)
	}
	entries, _, err := storage.Scan(ctx, f.service.store, diagnostics.BatchPrefix, "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("event batches = %d, error = %v", len(entries), err)
	}
}

func TestDiagnosticSourceWakeQueueCountsDroppedHints(t *testing.T) {
	s := &Service{diagnosticSourceWakes: make(chan diagnosticSourceWake, 1), diagnosticSourcePending: make(map[string]struct{})}
	s.queueDiagnosticSourceWake("tasks/state/task-a.json", time.Now())
	s.queueDiagnosticSourceWake("tasks/state/task-a.json", time.Now())
	s.queueDiagnosticSourceWake("tasks/state/task-b.json", time.Now())
	if got := s.diagnosticSourceDrops.Load(); got != 1 {
		t.Fatalf("dropped source wake count = %d, want 1", got)
	}
}

func sourceEntryVersionFromStore(t *testing.T, store storage.Store, key string) string {
	t.Helper()
	entry, err := store.Head(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return sourceEntryVersion(entry)
}
