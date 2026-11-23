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
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(job.DiagnosticOutbox[0]), true); err != nil {
		t.Fatalf("flush admitted source outbox: %v", err)
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

func TestDiagnosticSourceOutboxesKeepCriticalReserveSeparate(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	now := time.Now().UTC()
	jobs := []workqueue.Job{
		{
			SchemaVersion: 1, ID: workqueue.ID("notify", "ordinary-before-critical"), Kind: "notify", State: "pending",
			DiagnosticOutbox: []diagnostics.Event{{ID: diagnostics.ID("event", "ordinary-before-critical"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"}},
		},
		{
			SchemaVersion: 1, ID: workqueue.ID("notify", "critical-after-ordinary"), Kind: "notify", State: "pending",
			DiagnosticOutbox: []diagnostics.Event{{ID: diagnostics.ID("event", "critical-after-ordinary"), OccurredAt: now.Add(time.Second), Component: "background", Operation: "notify", Result: "failed"}},
		},
	}
	for _, job := range jobs {
		key := workqueue.Key(job.Kind, job.ID)
		if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := f.service.collectDiagnosticSourceOutbox(ctx, key, now); err != nil {
			t.Fatalf("admit source %s: %v", key, err)
		}
	}

	date := diagnostics.EventBatchDate(jobs[0].DiagnosticOutbox[0])
	pending, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date))
	if err != nil || pending.Batch == nil || !pending.Batch.Critical || len(pending.Batch.Events) != 1 || pending.Batch.Events[0].ID != jobs[1].DiagnosticOutbox[0].ID {
		t.Fatalf("critical pending batch = %+v, error = %v", pending, err)
	}
	entries, _, err := storage.Scan(ctx, f.service.store, diagnostics.BatchPrefix, "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ordinary batch was not flushed separately: batches=%d error=%v", len(entries), err)
	}
	ordinary, _, err := inventory.Read[diagnostics.EventBatch](ctx, f.service.store, entries[0].Key)
	if err != nil || len(ordinary.Events) != 1 || diagnosticCriticalEvent(ordinary.Events[0]) {
		t.Fatalf("ordinary batch contains critical events: %+v, error = %v", ordinary, err)
	}
}

func TestDiagnosticSourceBatchFlushesAtCriticalDeadline(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	event := diagnostics.Event{
		ID: diagnostics.ID("event", "critical-deadline"), OccurredAt: time.Now().UTC(),
		Component: "background", Operation: "notify", Result: "failed",
	}
	job := workqueue.Job{
		SchemaVersion: 1, ID: workqueue.ID("notify", "critical-deadline"), Kind: "notify", State: "pending",
		DiagnosticOutbox: []diagnostics.Event{event},
	}
	key := workqueue.Key(job.Kind, job.ID)
	if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Now().UTC().Add(-f.service.diagnosticsConfig().CriticalEventFlushInterval - time.Second)
	if err := f.service.collectDiagnosticSourceOutbox(ctx, key, admittedAt); err != nil {
		t.Fatalf("admit critical source outbox: %v", err)
	}
	if err := f.service.flushDueDiagnosticBatches(ctx, time.Now().UTC()); err != nil {
		t.Fatalf("flush due critical batch: %v", err)
	}
	receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(key)+".json")
	if err != nil || !diagnosticOutboxSettled(key, sourceEntryVersionFromStore(t, f.service.store, key), job.DiagnosticOutbox, receipt) {
		t.Fatalf("critical source receipt = %+v, error = %v", receipt, err)
	}
}

func TestDiagnosticOutboxHandoffWaitsForCommittedDisposition(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	events := []diagnostics.Event{{
		ID: diagnostics.ID("event", "cleanup-handoff"), OccurredAt: time.Now().UTC(),
		Component: "tasks", Operation: "cleanup", Result: "done",
	}}
	source := "tasks/state/cleanup-handoff.json"
	if err := f.service.handoffDiagnosticOutbox(ctx, source, events); err != errSourceBudgetYield {
		t.Fatalf("handoff before batch flush = %v, want retryable yield", err)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(events[0]), true); err != nil {
		t.Fatalf("flush cleanup handoff batch: %v", err)
	}
	if err := f.service.handoffDiagnosticOutbox(ctx, source, events); err != nil {
		t.Fatalf("handoff after committed disposition: %v", err)
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
