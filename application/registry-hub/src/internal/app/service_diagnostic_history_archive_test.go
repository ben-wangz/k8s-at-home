package app

import (
	"context"
	"encoding/json"
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

func TestDiagnosticReceiptArchiveResumesOutboxAndSkipsCompletedVersion(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	jobID := strings.Repeat("a", 32)
	sourceKey := workqueue.ReceiptKey("notify", jobID)
	payload, err := json.Marshal(blobNotification{IntentID: strings.Repeat("b", 32)})
	if err != nil {
		t.Fatal(err)
	}
	job := workqueue.Job{ID: jobID, Kind: "notify", State: "done", Payload: payload}
	for index := 0; index < 12; index++ {
		diagnostics.Record(&job.DiagnosticOutbox, diagnostics.Event{
			Component: "test", Operation: "archive", EntityID: job.ID,
			Stage: fmt.Sprintf("stage-%02d", index), Result: "done", OccurredAt: time.Now().UTC().Add(time.Duration(index) * time.Second),
		})
	}
	if err := inventory.Update(ctx, f.service.store, sourceKey, func(current *workqueue.Job) error {
		*current = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, sourceVersion, err := inventory.Read[workqueue.Job](ctx, f.service.store, sourceKey)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Head(ctx, sourceKey)
	if err != nil || entry.Version != sourceVersion {
		t.Fatalf("head source receipt = %+v, version=%q, error=%v", entry, sourceVersion, err)
	}
	fault := &receiptArchiveFaultStore{
		Store: f.service.store, collectedKey: diagnostics.Prefix + "collected/" + diagnostics.ID(sourceKey) + ".json", failCollectedWrite: 1,
	}
	f.service.store = fault

	var progress sourceSweepCheckpoint
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &progress); !errors.Is(err, errSourceBudgetYield) {
		t.Fatalf("first archive result = %v, want pending batch yield", err)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(job.DiagnosticOutbox[0]), true); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("first batch handoff error = %v, want injected failure", err)
	}
	stateKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(sourceKey) + ".json"
	state, _, err := inventory.Read[diagnosticReceiptArchive](ctx, fault, stateKey)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastArchivedEventID != "" || state.OutboxComplete || !state.NotificationIndexed || !state.TransferProjected {
		t.Fatalf("checkpoint did not preserve completed archive stages: %+v", state)
	}
	batches, _, err := fault.List(ctx, diagnostics.BatchPrefix, "", 10)
	if err != nil || len(batches) != 1 {
		t.Fatalf("durable event batches = %d, error = %v; want one batch before handoff confirmation", len(batches), err)
	}

	fault.failCollectedWrite = 0
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(job.DiagnosticOutbox[0]), true); err != nil {
		t.Fatalf("resume pending batch handoff: %v", err)
	}
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &progress); err != nil {
		t.Fatalf("resume receipt archive: %v", err)
	}
	state, _, err = inventory.Read[diagnosticReceiptArchive](ctx, fault, stateKey)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Complete || !state.OutboxComplete || state.LastArchivedEventID != job.DiagnosticOutbox[len(job.DiagnosticOutbox)-1].ID {
		t.Fatalf("archive did not complete all outbox events: %+v", state)
	}
	batches, _, err = fault.List(ctx, diagnostics.BatchPrefix, "", 10)
	if err != nil || len(batches) != 1 {
		t.Fatalf("archived batches = %d, error = %v; want one idempotently reused batch", len(batches), err)
	}

	fault.rejectSourceGet = sourceKey
	var skipped sourceSweepCheckpoint
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &skipped); err != nil {
		t.Fatalf("skip unchanged completed receipt: %v", err)
	}
	if skipped.Status.Counts["skipped"] != 1 {
		t.Fatalf("completed receipt skip count = %d, want 1", skipped.Status.Counts["skipped"])
	}
}

func TestHistoryCleanupReceiptArchiveUsesFilesystemMetadataScan(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	now := time.Now().UTC()
	jobID := strings.Repeat("d", 32)
	sourceKey := workqueue.ReceiptKey("history_cleanup", jobID)
	payload, err := json.Marshal(historyCleanupJobPayload{PreviewID: strings.Repeat("e", 32), Principal: "history-operator", PolicyVersion: "history-cleanup-v1"})
	if err != nil {
		t.Fatal(err)
	}
	job := workqueue.Job{
		SchemaVersion: 1, ID: jobID, Kind: "history_cleanup", State: "done", Payload: payload,
		CreatedAt: now.Add(-time.Minute), CompletedAt: now,
	}
	diagnostics.Record(&job.DiagnosticOutbox, diagnostics.Event{
		Component: "history_cleanup", Operation: "execute", EntityID: jobID,
		Stage: "done", Result: "done", OccurredAt: now,
	})
	if err := inventory.Update(ctx, f.service.store, sourceKey, func(current *workqueue.Job) error {
		*current = job
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	entries, _, err := storage.Scan(ctx, f.service.store, sourceKey, "", 1)
	if err != nil || len(entries) != 1 || entries[0].Version != "" || entries[0].MetadataFingerprint == "" {
		t.Fatalf("filesystem metadata scan = %+v, error = %v", entries, err)
	}
	var progress sourceSweepCheckpoint
	if more := f.service.sourceSweepEntity(ctx, "history_receipts", sourceKey, &progress, f.service.archiveDiagnosticReceipt); more {
		t.Fatal("one-receipt history sweep unexpectedly had more entities")
	}
	if progress.Status.Counts["archived"] != 0 {
		t.Fatalf("archive counted before outbox commit: %#v", progress.Status.Counts)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(job.DiagnosticOutbox[0]), true); err != nil {
		t.Fatalf("flush pending history receipt events: %v", err)
	}
	if more := f.service.sourceSweepEntity(ctx, "history_receipts", sourceKey, &progress, f.service.archiveDiagnosticReceipt); more {
		t.Fatal("one-receipt history sweep unexpectedly had more entities after resume")
	}
	if progress.Status.Counts["processed"] != 1 || progress.Status.Counts["archived"] != 1 {
		t.Fatalf("history sweep status = %#v, want one archived receipt", progress.Status.Counts)
	}

	projectionKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(sourceKey) + ".json"
	projection, _, err := inventory.Read[diagnosticReceiptArchive](ctx, f.service.store, projectionKey)
	if err != nil || !projection.Complete || projection.SourceFingerprint != entries[0].MetadataFingerprint {
		t.Fatalf("receipt archive projection = %+v, error = %v", projection, err)
	}
	collectionKey := diagnostics.Prefix + "collected/" + diagnostics.ID(sourceKey) + ".json"
	collected, _, err := inventory.Read[eventCollectionReceipt](ctx, f.service.store, collectionKey)
	if err != nil || !diagnosticOutboxSettled(sourceKey, projection.SourceVersion, job.DiagnosticOutbox, collected) {
		t.Fatalf("outbox collection receipt = %+v, error = %v", collected, err)
	}
	history, _, err := inventory.Read[terminalWorkHistoryRecord](ctx, f.service.store, terminalWorkHistoryKey(job.Kind, job.ID))
	if err != nil || history.View.Job.State != "done" || !history.TerminalAt.Equal(now) {
		t.Fatalf("terminal work history = %+v, error = %v", history, err)
	}
}

type receiptArchiveFaultStore struct {
	storage.Store
	collectedKey       string
	failCollectedWrite int
	collectedWrites    int
	rejectSourceGet    string
}

func (s *receiptArchiveFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == s.rejectSourceGet {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *receiptArchiveFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if key == s.collectedKey {
		s.collectedWrites++
		if s.failCollectedWrite > 0 && s.collectedWrites == s.failCollectedWrite {
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}
