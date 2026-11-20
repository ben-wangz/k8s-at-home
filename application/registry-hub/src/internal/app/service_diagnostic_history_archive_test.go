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
	entry := storage.Entry{Key: sourceKey, Version: sourceVersion}
	fault := &receiptArchiveFaultStore{
		Store: f.service.store, collectedKey: diagnostics.Prefix + "collected/" + diagnostics.ID(sourceKey) + ".json", failCollectedWrite: 2,
	}
	f.service.store = fault

	var progress sourceSweepCheckpoint
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &progress); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("first archive error = %v, want injected failure", err)
	}
	stateKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(sourceKey) + ".json"
	state, _, err := inventory.Read[diagnosticReceiptArchive](ctx, fault, stateKey)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastArchivedEventID != job.DiagnosticOutbox[3].ID || state.OutboxComplete || !state.NotificationIndexed || !state.TransferProjected {
		t.Fatalf("checkpoint did not preserve completed archive stages: %+v", state)
	}

	fault.failCollectedWrite = 0
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
	events, _, err := fault.List(ctx, diagnostics.Prefix+"events/", "", 100)
	if err != nil || len(events) != len(job.DiagnosticOutbox) {
		t.Fatalf("archived events = %d, error = %v; want %d", len(events), err, len(job.DiagnosticOutbox))
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
