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

func TestDiagnosticSourceVersionChangesCanCommitPendingOutbox(t *testing.T) {
	for _, test := range []struct {
		name        string
		keepOld     bool
		gapExpected bool
	}{
		{name: "append after acknowledged events", keepOld: true},
		{name: "continue after outbox rotation", gapExpected: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			now := time.Now().UTC()
			first := diagnostics.Event{ID: diagnostics.ID("event", "version-first"), OccurredAt: now, Component: "background", Operation: "notify", Result: "failed"}
			second := diagnostics.Event{ID: diagnostics.ID("event", "version-second"), OccurredAt: now, Component: "background", Operation: "notify", Result: "failed"}
			job := workqueue.Job{SchemaVersion: 1, ID: workqueue.ID("notify", "version-change"), Kind: "notify", State: "pending", DiagnosticOutbox: []diagnostics.Event{first}}
			key := workqueue.Key(job.Kind, job.ID)
			if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
				t.Fatal(err)
			}
			firstVersion := sourceEntryVersionFromStore(t, f.service.store, key)
			if err := f.service.collectDiagnosticOutboxVersion(ctx, key, firstVersion, job.DiagnosticOutbox); err != nil {
				t.Fatalf("admit original outbox: %v", err)
			}
			if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(first), true); err != nil {
				t.Fatalf("commit original outbox: %v", err)
			}

			job.DiagnosticOutbox = []diagnostics.Event{second}
			if test.keepOld {
				job.DiagnosticOutbox = append([]diagnostics.Event{first}, job.DiagnosticOutbox...)
			}
			if err := inventory.Update(ctx, f.service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
				t.Fatal(err)
			}
			secondVersion := sourceEntryVersionFromStore(t, f.service.store, key)
			if secondVersion == firstVersion {
				t.Fatal("source version did not change")
			}
			if err := f.service.collectDiagnosticOutboxVersion(ctx, key, secondVersion, job.DiagnosticOutbox); err != nil {
				t.Fatalf("admit changed-version outbox: %v", err)
			}
			if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(second), true); err != nil {
				t.Fatalf("commit changed-version outbox: %v", err)
			}
			receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(key)+".json")
			if err != nil || !diagnosticOutboxSettled(key, secondVersion, job.DiagnosticOutbox, receipt) {
				t.Fatalf("changed-version source is not settled: receipt=%+v error=%v", receipt, err)
			}
			if (len(receipt.GapIDs) > 0) != test.gapExpected {
				t.Fatalf("source gap IDs = %v, want gap=%v", receipt.GapIDs, test.gapExpected)
			}
		})
	}
}

func TestDiagnosticSourceOutboxesCoalesceAcrossKeysAndRecoverCommittedBatch(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	jobs := []workqueue.Job{
		{
			SchemaVersion: 1, ID: workqueue.ID("notify", "coalesce-a"), Kind: "notify", State: "pending",
			DiagnosticOutbox: []diagnostics.Event{{ID: diagnostics.ID("event", "coalesce-a"), OccurredAt: time.Now().UTC(), Component: "background", Operation: "notify", Result: "failed"}},
		},
		{
			SchemaVersion: 1, ID: workqueue.ID("notify", "coalesce-b"), Kind: "notify", State: "pending",
			DiagnosticOutbox: []diagnostics.Event{{ID: diagnostics.ID("event", "coalesce-b"), OccurredAt: time.Now().UTC(), Component: "background", Operation: "notify", Result: "failed"}},
		},
	}
	keys := make([]string, len(jobs))
	for index, job := range jobs {
		keys[index] = workqueue.Key(job.Kind, job.ID)
		if err := inventory.Update(ctx, f.service.store, keys[index], func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := f.service.collectDiagnosticSourceOutbox(ctx, keys[index], time.Now().UTC()); err != nil {
			t.Fatalf("admit source %s: %v", keys[index], err)
		}
	}

	date := diagnostics.EventBatchDate(jobs[0].DiagnosticOutbox[0])
	pending, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date))
	if err != nil || pending.Batch == nil || len(pending.Batch.Events) != 2 || len(pending.Batch.Sources) != 2 {
		t.Fatalf("pending cross-source batch = %+v, error = %v", pending, err)
	}
	committed, err := (diagnostics.BatchLog{
		Store: f.service.store, Policy: f.service.diagnosticBatchPolicy(), Retention: f.service.diagnosticsConfig().EventRetention,
	}).Append(ctx, pending.Batch.Events, pending.Batch.Critical)
	if err != nil {
		t.Fatalf("simulate batch commit before source acknowledgement: %v", err)
	}

	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); err != nil {
		t.Fatalf("recover pending acknowledgement after restart: %v", err)
	}
	entries, _, err := storage.Scan(ctx, f.service.store, diagnostics.BatchPrefix, "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovered event batches = %d, error = %v", len(entries), err)
	}
	for index, key := range keys {
		receipt, _, readErr := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(key)+".json")
		if readErr != nil || !diagnosticOutboxSettled(key, sourceEntryVersionFromStore(t, f.service.store, key), jobs[index].DiagnosticOutbox, receipt) {
			t.Fatalf("source %s not settled after recovery: %+v, error = %v", key, receipt, readErr)
		}
		if len(receipt.Dispositions) != 1 || receipt.Dispositions[0].BatchID != committed.ID || receipt.Dispositions[0].BatchSHA256 != committed.ContentSHA256 {
			t.Fatalf("source %s batch association = %+v, want %s/%s", key, receipt.Dispositions, committed.ID, committed.ContentSHA256)
		}
	}
}
