package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
)

func TestHistoryCleanupHandoffRecoversInBackground(t *testing.T) {
	for _, failure := range []string{"queue", "idempotency"} {
		t.Run(failure, func(t *testing.T) {
			testHistoryCleanupInterruptedSubmission(t, failure)
		})
	}
}

func testHistoryCleanupInterruptedSubmission(t *testing.T, failure string) {
	t.Helper()
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	fault := &historyCleanupHandoffFaultStore{Store: base}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, fault).service
	s.cfg.Diagnostics.HistoryCleanupPreviewTTL = time.Minute
	id := diagnostics.ID("interrupted-cleanup", failure)
	preview, err := s.createHistoryCleanupPreview(ctx, "operator", historyCleanupPreviewRequest{
		Categories: []string{"terminal_transfer_history"}, Cutoff: time.Now().UTC().Add(-72 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := historyCleanupExecutionRequest{PreviewID: preview.ID}
	hash := historyCleanupRequestHash(preview.ID, "")
	source := diagnostics.ID("history-cleanup-request", "operator", id, hash)
	jobID := workqueue.ID(historyCleanupWorkKind, source)
	key := historyCleanupOperationKey(jobID)
	markerKey := historyCleanupIdempotencyPrefix + diagnostics.ID("history-cleanup-idempotency", "operator", id) + ".json"
	fault.failKey = workqueue.Key(historyCleanupWorkKind, jobID)
	if failure == "idempotency" {
		fault.failKey = markerKey
	}
	if _, status, err := s.submitHistoryCleanup(ctx, "operator", id, request); status != http.StatusServiceUnavailable || !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("submission did not reach injected failure: status=%d err=%v", status, err)
	}
	if _, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("queue job before recovery: %v", err)
	}
	entry, err := s.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pruneHistoryCleanupOperationEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if failure == "queue" {
		assertRecoveredHistoryCleanup(t, s, jobID, id, request)
		return
	}
	if _, err := s.store.Head(ctx, key); err != nil {
		t.Fatalf("uncommitted operation was removed before its grace period: %v", err)
	}
	if _, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("uncommitted operation was queued: %v", err)
	}
	if err := inventory.Update(ctx, s.store, key, func(op *historyCleanupOperation) error {
		op.CreatedAt = time.Now().Add(-2 * time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneHistoryCleanupOperationEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("uncommitted operation still occupies a slot: %v", err)
	}
	if _, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired uncommitted operation was queued: %v", err)
	}
	if _, status, err := s.submitHistoryCleanup(ctx, "operator", id, request); status != http.StatusAccepted || err != nil {
		t.Fatalf("replacement submission after reclamation: status=%d err=%v", status, err)
	}
}

func assertRecoveredHistoryCleanup(t *testing.T, s *Service, jobID, idempotencyKey string, request historyCleanupExecutionRequest) {
	t.Helper()
	ctx := context.Background()
	job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobID)
	if err != nil || job.State != "pending" {
		t.Fatalf("background sweep did not restore the job: %+v err=%v", job, err)
	}
	view, status, err := s.submitHistoryCleanup(ctx, "operator", idempotencyKey, request)
	if err != nil || status != http.StatusAccepted || view.ID != jobID {
		t.Fatalf("same request did not return recovered job: %+v status=%d err=%v", view, status, err)
	}
}

type historyCleanupHandoffFaultStore struct {
	storage.Store
	failKey string
}

func (s *historyCleanupHandoffFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if key == s.failKey {
		s.failKey = ""
		return storage.Entry{}, fmt.Errorf("interrupted cleanup handoff: %w", storage.ErrUnavailable)
	}
	return s.Store.Put(ctx, key, body, size, options)
}
