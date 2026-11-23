package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

type diagnosticFlushFaultStore struct {
	storage.Store
	failOutcomeWrites   int
	failIntentWrites    int
	unknownBatchCommits int
}

func (s *diagnosticFlushFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return storage.Entry{}, err
	}
	if strings.HasPrefix(key, diagnosticPendingBatchPrefix) && (s.failOutcomeWrites > 0 || s.failIntentWrites > 0) {
		var partition diagnosticPendingPartition
		if json.Unmarshal(data, &partition) == nil && partition.Batch != nil {
			if partition.Batch.FlushOutcome != nil && s.failOutcomeWrites > 0 {
				s.failOutcomeWrites--
				return storage.Entry{}, storage.ErrUnavailable
			}
			if partition.Batch.FlushIntent != nil && s.failIntentWrites > 0 {
				s.failIntentWrites--
				return storage.Entry{}, storage.ErrUnavailable
			}
		}
	}
	entry, err := s.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if err == nil && strings.HasPrefix(key, diagnostics.BatchPrefix) && s.unknownBatchCommits > 0 {
		s.unknownBatchCommits--
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, err
}

func TestDiagnosticFlushIntentPreservesBatchIdentityAfterOutcomeWriteFailure(t *testing.T) {
	for _, commitMode := range []struct {
		name               string
		unknownAfterCommit int
	}{
		{name: "confirmed batch PUT"},
		{name: "unknown batch PUT commit", unknownAfterCommit: 1},
	} {
		for _, budget := range []struct {
			name          string
			dailyBatches  int64
			wantBatches   int
			wantSecondAck string
		}{
			{name: "budget available", dailyBatches: 4, wantBatches: 2, wantSecondAck: "archived"},
			{name: "one batch budget", dailyBatches: 1, wantBatches: 1, wantSecondAck: "discarded_by_policy"},
		} {
			t.Run(commitMode.name+"/"+budget.name, func(t *testing.T) {
				base := newFilesystemStore(t)
				faults := &diagnosticFlushFaultStore{
					Store: base, failOutcomeWrites: 1, unknownBatchCommits: commitMode.unknownAfterCommit,
				}
				f := newStagedAcceptanceFixtureWithStore(t, 1<<20, faults)
				f.service.cfg.Diagnostics.EventBatchDailyCount = budget.dailyBatches
				ctx := context.Background()
				now := time.Now().UTC()
				first := diagnostics.Event{ID: diagnostics.ID("event", commitMode.name+budget.name+"-first"), OccurredAt: now, Component: "background", Operation: "notify", Result: "failed"}
				trigger := diagnostics.Event{ID: diagnostics.ID("event", commitMode.name+budget.name+"-trigger"), OccurredAt: now.Add(time.Millisecond), Component: "background", Operation: "notify", Result: "pending"}
				second := diagnostics.Event{ID: diagnostics.ID("event", commitMode.name+budget.name+"-second"), OccurredAt: now.Add(2 * time.Millisecond), Component: "background", Operation: "notify", Result: "failed"}
				firstSource := workqueue.Key("notify", commitMode.name+budget.name+"-first")
				triggerSource := workqueue.Key("notify", commitMode.name+budget.name+"-trigger")
				secondSource := workqueue.Key("notify", commitMode.name+budget.name+"-second")
				if err := f.service.collectDiagnosticOutboxVersion(ctx, firstSource, "first-v1", []diagnostics.Event{first}); err != nil {
					t.Fatalf("admit first critical event: %v", err)
				}
				if err := f.service.collectDiagnosticOutboxVersion(ctx, triggerSource, "trigger-v1", []diagnostics.Event{trigger}); !errors.Is(err, storage.ErrUnavailable) {
					t.Fatalf("priority-triggered flush error = %v, want outcome write failure", err)
				}

				date := diagnostics.EventBatchDate(first)
				partition, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date))
				if err != nil || partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Batch == nil || partition.Batch.FlushIntent == nil || partition.Batch.FlushOutcome != nil {
					t.Fatalf("pending flush intent = %+v, error = %v", partition.Batch, err)
				}
				assertDiagnosticPendingReadableByPreviousWriter(t, ctx, base, date, now, true)
				if err := f.service.collectDiagnosticOutboxVersion(ctx, secondSource, "second-v1", []diagnostics.Event{second}); err != nil {
					t.Fatalf("admit second critical event after retrying frozen batch: %v", err)
				}
				if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); err != nil {
					t.Fatalf("flush second critical event: %v", err)
				}

				entries, _, err := storage.Scan(ctx, f.service.store, diagnostics.BatchPrefix+date+"/", "", 10)
				if err != nil || len(entries) != budget.wantBatches {
					t.Fatalf("stored batches = %d, error = %v, want %d", len(entries), err, budget.wantBatches)
				}
				firstCopies, secondCopies := 0, 0
				firstBatchID := ""
				for _, entry := range entries {
					batch, _, err := inventory.Read[diagnostics.EventBatch](ctx, f.service.store, entry.Key)
					if err != nil {
						t.Fatalf("read batch %q: %v", entry.Key, err)
					}
					if err := diagnostics.VerifyEventBatch(batch, entry.Key); err != nil {
						t.Fatalf("verify batch %q: %v", entry.Key, err)
					}
					for _, event := range batch.Events {
						switch event.ID {
						case first.ID:
							firstCopies++
							firstBatchID = batch.ID
						case second.ID:
							secondCopies++
						}
					}
				}
				if firstCopies != 1 {
					t.Fatalf("first event copies = %d, want exactly one", firstCopies)
				}
				wantSecondCopies := 1
				if budget.wantSecondAck == "discarded_by_policy" {
					wantSecondCopies = 0
				}
				if secondCopies != wantSecondCopies {
					t.Fatalf("second event copies = %d, want %d", secondCopies, wantSecondCopies)
				}

				firstReceipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(firstSource)+".json")
				if err != nil || len(firstReceipt.Dispositions) != 1 || firstReceipt.Dispositions[0].Disposition != "archived" || firstReceipt.Dispositions[0].BatchID != firstBatchID {
					t.Fatalf("first source receipt = %+v, error = %v", firstReceipt, err)
				}
				secondReceipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(secondSource)+".json")
				if err != nil || len(secondReceipt.Dispositions) != 1 || secondReceipt.Dispositions[0].Disposition != budget.wantSecondAck {
					t.Fatalf("second source receipt = %+v, error = %v, want %s", secondReceipt, err, budget.wantSecondAck)
				}
				if budget.wantSecondAck == "discarded_by_policy" && secondReceipt.Dispositions[0].Reason != "batch_budget_capacity" {
					t.Fatalf("second source policy reason = %q, want batch_budget_capacity", secondReceipt.Dispositions[0].Reason)
				}
			})
		}
	}
}

func TestDiagnosticLegacyPendingBatchSettlesBeforeSamePriorityAdmission(t *testing.T) {
	base := newFilesystemStore(t)
	faults := &diagnosticFlushFaultStore{Store: base, failIntentWrites: 1}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, faults)
	f.service.cfg.Diagnostics.EventBatchDailyCount = 1
	f.service.cfg.Diagnostics.EventBatchDailyCriticalCount = 1
	ctx := context.Background()
	now := time.Now().UTC()
	first := diagnostics.Event{ID: diagnostics.ID("event", "legacy-flushed"), OccurredAt: now, Component: "background", Operation: "notify", Result: "failed"}
	second := first
	second.ID = diagnostics.ID("event", "new-after-upgrade")
	date := diagnostics.EventBatchDate(first)
	firstSource := workqueue.Key("notify", "legacy-flushed")
	secondSource := workqueue.Key("notify", "new-after-upgrade")
	legacy := diagnosticPendingPartition{SchemaVersion: 1, Date: date, Batch: &diagnosticPendingBatch{
		Date: date, Critical: true, FirstAdmittedAt: now, CriticalDeadline: now.Add(time.Hour),
		Events: []diagnostics.Event{first}, Sources: []diagnosticPendingSource{{
			SourceKey: firstSource, SourceVersion: "first-v1", FirstEventID: first.ID, LastEventID: first.ID, EventCount: 1,
		}},
	}}
	key := diagnosticPendingBatchKey(date)
	if err := inventory.Update(ctx, base, key, func(current *diagnosticPendingPartition) error {
		*current = legacy
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	log := diagnostics.BatchLog{Store: base, Policy: f.service.diagnosticBatchPolicy()}
	committed, err := log.Append(ctx, legacy.Batch.Events, true)
	if err != nil {
		t.Fatal(err)
	}
	before, err := base.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.collectDiagnosticOutboxVersion(ctx, secondSource, "second-v1", []diagnostics.Event{second}); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("admission with failed seal = %v, want storage unavailable", err)
	}
	after, err := base.Head(ctx, key)
	if err != nil || before.Version != after.Version {
		t.Fatalf("failed seal changed legacy partition: before %q, after %q, error %v", before.Version, after.Version, err)
	}
	if err := f.service.collectDiagnosticOutboxVersion(ctx, secondSource, "second-v1", []diagnostics.Event{second}); err != nil {
		t.Fatalf("resume original batch before admitting the next source: %v", err)
	}
	assertDiagnosticPendingReadableByPreviousWriter(t, ctx, base, date, now, false)
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); err != nil {
		t.Fatal(err)
	}
	firstReceipt, _, err := inventory.Read[eventCollectionReceipt](ctx, base, diagnostics.Prefix+"collected/"+diagnostics.ID(firstSource)+".json")
	if err != nil || len(firstReceipt.Dispositions) != 1 || firstReceipt.Dispositions[0].Disposition != "archived" ||
		firstReceipt.Dispositions[0].BatchID != committed.ID || firstReceipt.Dispositions[0].BatchSHA256 != committed.ContentSHA256 {
		t.Fatalf("legacy source receipt = %+v, error = %v, want original archived batch", firstReceipt, err)
	}
	secondReceipt, _, err := inventory.Read[eventCollectionReceipt](ctx, base, diagnostics.Prefix+"collected/"+diagnostics.ID(secondSource)+".json")
	if err != nil || len(secondReceipt.Dispositions) != 1 || secondReceipt.Dispositions[0].Disposition != "discarded_by_policy" ||
		secondReceipt.Dispositions[0].Reason != "batch_budget_capacity" {
		t.Fatalf("new source receipt = %+v, error = %v, want policy discard at capacity", secondReceipt, err)
	}
}

func assertDiagnosticPendingReadableByPreviousWriter(t *testing.T, ctx context.Context, store storage.Store, date string, now time.Time, wantDue bool) {
	t.Helper()
	type previousPartition struct {
		SchemaVersion int    `json:"schemaVersion"`
		Date          string `json:"date"`
		Batch         *struct {
			Critical         bool      `json:"critical"`
			OrdinaryDeadline time.Time `json:"ordinaryDeadline"`
			CriticalDeadline time.Time `json:"criticalDeadline"`
		} `json:"batch"`
	}
	partition, _, err := inventory.Read[previousPartition](ctx, store, diagnosticPendingBatchKey(date))
	if err != nil || partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil {
		t.Fatalf("previous writer cannot read pending partition %+v: %v", partition, err)
	}
	deadline := partition.Batch.OrdinaryDeadline
	if partition.Batch.Critical {
		deadline = partition.Batch.CriticalDeadline
	}
	if due := !deadline.After(now); due != wantDue {
		t.Fatalf("pending partition due for previous writer = %t, want %t", due, wantDue)
	}
}
