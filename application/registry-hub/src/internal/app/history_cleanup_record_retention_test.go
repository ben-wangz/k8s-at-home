package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestHistoryCleanupRetentionPrunesFailedRecordsWithoutNewSubmissions(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	s.cfg.Diagnostics.HistoryCleanupResultRetention = time.Minute
	now := time.Now().UTC()
	failedAt := now.Add(-2 * time.Minute)
	previewID := diagnostics.ID("expired-failed-preview")
	previewJob := enqueueFailedCleanupJob(t, s, historyCleanupPreviewKind, previewID,
		historyCleanupRangeJobPayload{PreviewID: previewID}, failedAt)
	operationJob := enqueueFailedCleanupJob(t, s, historyCleanupWorkKind, "expired-failed-operation",
		historyCleanupJobPayload{PreviewID: previewID, Principal: "operator"}, failedAt)
	previewKey := historyCleanupRangePreviewKey(previewID)
	operationKey := historyCleanupOperationKey(operationJob.ID)
	idempotencyID := diagnostics.ID("expired-failed-idempotency")
	idempotencyKey := historyCleanupIdempotencyPrefix + idempotencyID + ".json"

	fixtures := map[string]any{
		previewKey: historyCleanupRangePreview{SchemaVersion: 1, ID: previewID, ExpiresAt: failedAt},
		operationKey: historyCleanupOperation{
			SchemaVersion: 1, ID: operationJob.ID, PreviewID: previewID, Principal: "operator",
			Status: "running", IdempotencyID: idempotencyID,
		},
		idempotencyKey: historyCleanupIdempotency{SchemaVersion: 1, OperationID: operationJob.ID},
	}
	for key, fixture := range fixtures {
		if err := putHistoryCleanupJSON(ctx, s.store, key, fixture, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	runHistoryCleanupRetentionSweep(s, ctx, "diagnostic_retention_cleanup_operations", historyCleanupOperationPrefix)
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, operationKey)
	if err != nil || operation.Status != "failed" || !operation.ExpiresAt.Equal(failedAt.Add(time.Minute)) {
		t.Fatalf("failed operation did not receive the expected TTL: %+v, %v", operation, err)
	}
	if _, err := s.historyCleanupStatus(ctx, operationJob.ID, "operator"); !errors.Is(err, inventory.ErrExpired) {
		t.Fatalf("expired failed result status: %v", err)
	}
	runHistoryCleanupRetentionSweep(s, ctx, "diagnostic_retention_cleanup_operations", historyCleanupOperationPrefix)
	entry, err := s.store.Head(ctx, previewKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pruneHistoryCleanupRangeEntry(ctx, entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{previewKey, operationKey, idempotencyKey,
		workqueue.Key(previewJob.Kind, previewJob.ID), workqueue.Key(operationJob.Kind, operationJob.ID)} {
		if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("expired record %q remains: %v", key, err)
		}
	}
}

func TestHistoryCleanupRetentionPrunesOrphansAndKeepsActiveJobs(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	now := time.Now().UTC()
	idempotencyKey := historyCleanupIdempotencyPrefix + "expired-orphan.json"
	if err := putHistoryCleanupJSON(ctx, s.store, idempotencyKey,
		historyCleanupIdempotency{SchemaVersion: 1, OperationID: "missing", ExpiresAt: now.Add(-time.Hour)}, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	failed := enqueueFailedCleanupJob(t, s, historyCleanupWorkKind, "missing-parent", historyCleanupJobPayload{}, now.Add(-time.Hour))
	active, err := s.backgroundQueue().Enqueue(ctx, historyCleanupWorkKind, "active", historyCleanupJobPayload{}, now)
	if err != nil {
		t.Fatal(err)
	}
	runHistoryCleanupRetentionSweep(s, ctx, "diagnostic_retention_cleanup_idempotency", historyCleanupIdempotencyPrefix)
	runHistoryCleanupRetentionSweep(s, ctx, "diagnostic_retention_cleanup_failed_operations", workqueue.Prefix+"jobs/"+historyCleanupWorkKind+"/")
	for _, key := range []string{idempotencyKey, workqueue.Key(failed.Kind, failed.ID)} {
		if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("orphaned record %q remains: %v", key, err)
		}
	}
	if _, err := s.store.Head(ctx, workqueue.Key(active.Kind, active.ID)); err != nil {
		t.Fatalf("pending job was removed: %v", err)
	}
}

func enqueueFailedCleanupJob(t *testing.T, s *Service, kind, source string, payload any, at time.Time) workqueue.Job {
	t.Helper()
	ctx := context.Background()
	job, err := s.backgroundQueue().Enqueue(ctx, kind, source, payload, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(ctx, s.store, workqueue.Key(kind, job.ID), func(current *workqueue.Job) error {
		current.State, current.ErrorAt = "failed", at
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return job
}

func runHistoryCleanupRetentionSweep(s *Service, ctx context.Context, kind, prefix string) {
	var p sourceSweepCheckpoint
	s.sourceSweepBatch(ctx, kind, prefix, &p, func(ctx context.Context, entry storage.Entry, _ *sourceSweepCheckpoint) error {
		return s.pruneHistoryCleanupRecord(ctx, kind, entry)
	})
}
