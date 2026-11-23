package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

type diagnosticDispositionFaultStore struct {
	storage.Store
	collectedWrites int
	failCollectedAt int
}

func (s *diagnosticDispositionFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.HasPrefix(key, diagnostics.Prefix+"collected/") {
		s.collectedWrites++
		if s.failCollectedAt > 0 && s.collectedWrites == s.failCollectedAt {
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestDiagnosticLegacyPendingPartialAcknowledgementConvergesAfterWatermark(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	now := time.Now().UTC()
	date := now.Format("20060102")
	events := []diagnostics.Event{
		{ID: diagnostics.ID("event", "legacy-partial-a"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"},
		{ID: diagnostics.ID("event", "legacy-partial-b"), OccurredAt: now.Add(time.Millisecond), Component: "background", Operation: "notify", Result: "pending"},
	}
	sources := []diagnosticPendingSource{
		{SourceKey: workqueue.Key("notify", "legacy-partial-a"), SourceVersion: "source-a", FirstEventID: events[0].ID, LastEventID: events[0].ID, EventCount: 1},
		{SourceKey: workqueue.Key("notify", "legacy-partial-b"), SourceVersion: "source-b", FirstEventID: events[1].ID, LastEventID: events[1].ID, EventCount: 1},
	}
	committed, err := (diagnostics.BatchLog{Store: f.service.store, Policy: f.service.diagnosticBatchPolicy(), Retention: f.service.diagnosticsConfig().EventRetention}).Append(ctx, events, false)
	if err != nil {
		t.Fatal(err)
	}
	firstDisposition := diagnosticEventDisposition{SourceKey: sources[0].SourceKey, SourceVersion: sources[0].SourceVersion,
		FirstEventID: sources[0].FirstEventID, LastEventID: sources[0].LastEventID, EventCount: 1,
		Disposition: "archived", BatchID: committed.ID, BatchSHA256: committed.ContentSHA256}
	if err := f.service.saveDiagnosticPendingDisposition(ctx, sources[0], firstDisposition); err != nil {
		t.Fatal(err)
	}
	partition := diagnosticPendingPartition{SchemaVersion: 1, Date: date, Batch: &diagnosticPendingBatch{
		Date: date, Critical: false, FirstAdmittedAt: now, OrdinaryDeadline: now.Add(-time.Second), Events: events, Sources: sources,
	}}
	if err := f.service.saveDiagnosticPendingPartition(ctx, date, partition); err != nil {
		t.Fatal(err)
	}
	watermarkDate := now.Add(24 * time.Hour).Format("20060102")
	if err := diagnostics.AdvanceEventHistoryWatermark(ctx, f.service.store, watermarkDate); err != nil {
		t.Fatal(err)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, false); err != nil {
		t.Fatalf("settle legacy pending partition after watermark advance: %v", err)
	}
	for index, expected := range []string{"archived", "discarded_by_policy"} {
		receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(sources[index].SourceKey)+".json")
		if err != nil || len(receipt.Dispositions) != 1 || receipt.Dispositions[0].Disposition != expected {
			t.Fatalf("source %d disposition = %+v, error = %v, want %s", index, receipt.Dispositions, err, expected)
		}
		if index == 0 && (receipt.Dispositions[0].BatchID != committed.ID || receipt.Dispositions[0].BatchSHA256 != committed.ContentSHA256) {
			t.Fatalf("existing archived disposition changed: %+v", receipt.Dispositions[0])
		}
	}
	if _, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pending partition remains after recovery: %v", err)
	}
}

func TestDiagnosticFlushPersistsOutcomeBeforeAcknowledgingSources(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	fault := &diagnosticDispositionFaultStore{Store: f.service.store, failCollectedAt: 2}
	f.service.store = fault
	ctx := context.Background()
	now := time.Now().UTC().Add(-2 * time.Second)
	events := []diagnostics.Event{
		{ID: diagnostics.ID("event", "persist-outcome-a"), OccurredAt: now, Component: "background", Operation: "notify", Result: "pending"},
		{ID: diagnostics.ID("event", "persist-outcome-b"), OccurredAt: now.Add(time.Millisecond), Component: "background", Operation: "notify", Result: "pending"},
	}
	sources := []string{workqueue.Key("notify", "persist-outcome-a"), workqueue.Key("notify", "persist-outcome-b")}
	for index := range sources {
		if err := f.service.collectDiagnosticOutboxVersion(ctx, sources[index], fmt.Sprintf("source-%d", index), []diagnostics.Event{events[index]}); err != nil {
			t.Fatalf("admit source %d: %v", index, err)
		}
	}
	date := diagnostics.EventBatchDate(events[0])
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("partial flush error = %v, want injected receipt failure", err)
	}
	partition, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date))
	if err != nil || partition.Batch == nil || partition.Batch.FlushOutcome == nil || partition.Batch.FlushOutcome.Disposition != "archived" {
		t.Fatalf("persisted flush outcome = %+v, error = %v", partition.Batch, err)
	}
	first, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(sources[0])+".json")
	if err != nil || len(first.Dispositions) != 1 || first.Dispositions[0].Disposition != "archived" {
		t.Fatalf("first source receipt = %+v, error = %v", first, err)
	}
	f.service.cfg.Diagnostics.EventRetention = time.Second
	if err := f.service.flushDiagnosticPendingPartition(ctx, date, true); err != nil {
		t.Fatalf("resume persisted archive outcome after retention expired: %v", err)
	}
	for index, source := range sources {
		receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, diagnostics.Prefix+"collected/"+diagnostics.ID(source)+".json")
		if err != nil || len(receipt.Dispositions) != 1 || receipt.Dispositions[0].Disposition != "archived" ||
			receipt.Dispositions[0].BatchID != partition.Batch.FlushOutcome.BatchID || receipt.Dispositions[0].BatchSHA256 != partition.Batch.FlushOutcome.BatchSHA256 {
			t.Fatalf("source %d receipt = %+v, error = %v", index, receipt, err)
		}
	}
	if _, _, err := inventory.Read[diagnosticPendingPartition](ctx, f.service.store, diagnosticPendingBatchKey(date)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("pending partition remains after outcome recovery: %v", err)
	}
}
