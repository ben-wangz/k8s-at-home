package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
)

func TestHistoryCleanupRequestBudgetReservesCheckpointRequests(t *testing.T) {
	ctx := withHistoryCleanupRequestBudget(context.Background(), 17)
	if err := consumeHistoryCleanupStorageRequest(ctx); err != nil {
		t.Fatalf("consume request before reserved checkpoint budget: %v", err)
	}
	if err := consumeHistoryCleanupStorageRequest(ctx); !errors.Is(err, errHistoryCleanupRequestBudget) {
		t.Fatalf("ordinary work consumed reserved checkpoint request: %v", err)
	}
	checkpointCtx := withHistoryCleanupCheckpointBudget(ctx)
	for range 16 {
		if err := consumeHistoryCleanupStorageRequest(checkpointCtx); err != nil {
			t.Fatalf("consume checkpoint request: %v", err)
		}
	}
	if err := consumeHistoryCleanupStorageRequest(checkpointCtx); !errors.Is(err, errHistoryCleanupRequestBudget) {
		t.Fatalf("checkpoint exceeded total request budget: %v", err)
	}
}

func TestDiagnosticCollectionReceiptRequiresSettledHandoffProof(t *testing.T) {
	receipt := eventCollectionReceipt{
		SourceVersion: "source-v2", LastEventID: "event-2",
		Dispositions: []diagnosticEventDisposition{{
			SourceKey: "tasks/state/source.json", SourceVersion: "source-v2", FirstEventID: "event-1", LastEventID: "event-2",
			EventCount: 2, Disposition: "archived", BatchID: "batch-id", BatchSHA256: "batch-hash",
		}},
	}
	if !diagnosticCollectionReceiptSettled(receipt) {
		t.Fatal("complete archived handoff was not accepted")
	}
	receipt.Dispositions[0].Disposition = "pending"
	if diagnosticCollectionReceiptSettled(receipt) {
		t.Fatal("pending handoff was considered settled")
	}
	receipt.Dispositions[0].Disposition = "archived"
	receipt.Dispositions[0].BatchSHA256 = ""
	if diagnosticCollectionReceiptSettled(receipt) {
		t.Fatal("archived handoff without content proof was considered settled")
	}
}

func TestHistoryCleanupSnapshotContinuationKeepsManifestUntilChildrenSettle(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := inventory.ID("segmented-snapshot")
	prefix := storageInventoryPrefix + "snapshots/" + id + "/"
	sizes := map[string]int64{prefix + "page-a": 40 << 20, prefix + "page-b": 40 << 20}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, snapshotPageSizeStore{Store: store, sizes: sizes}).service
	expiresAt := time.Now().UTC().Add(-time.Hour)
	if err := putHistoryCleanupJSON(ctx, s.store, prefix+"manifest.json",
		storageSnapshot{SnapshotID: id, ExpiresAt: expiresAt}, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	for key := range sizes {
		if err := putHistoryCleanupJSON(ctx, s.store, key, []string{key}, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	request := historyCleanupRangeRequest{Category: "storage_snapshot_history", From: expiresAt.Add(-time.Minute), To: expiresAt.Add(time.Minute)}
	first := completeHistoryRangePreview(t, s, request)
	if first.SegmentItems != 1 || first.SegmentBytes != 40<<20 || first.NextCursor == "" {
		t.Fatalf("first segment=%+v", first)
	}
	request.Cursor = first.NextCursor
	second := completeHistoryRangePreview(t, s, request)
	if second.SegmentItems != 2 || second.SegmentBytes <= 40<<20 || second.NextCursor != "" {
		t.Fatalf("second segment=%+v", second)
	}
	for _, preview := range []historyCleanupRangePreview{first, second} {
		task, status, err := s.submitHistoryCleanup(ctx, "operator", "apply-"+preview.ID,
			historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: preview.SegmentID})
		if err != nil || status != http.StatusAccepted {
			t.Fatalf("submit: status=%d error=%v", status, err)
		}
		job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		for range 4 {
			_, done, err := s.historyCleanupStep(ctx, &job)
			if err != nil {
				t.Fatal(err)
			}
			if done {
				break
			}
		}
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(task.ID))
		if err != nil || operation.Status != "completed" || operation.Counts["storage_snapshot_history"].Deleted != int64(preview.SegmentItems) {
			t.Fatalf("execute: operation=%+v error=%v", operation, err)
		}
		_, err = s.store.Head(ctx, prefix+"manifest.json")
		if preview.ID == first.ID && err != nil || preview.ID == second.ID && !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("manifest after applying %s: %v", preview.ID, err)
		}
		_, err = s.store.Head(ctx, storageRetentionPrefix+id+".json")
		if preview.ID == first.ID && err != nil || preview.ID == second.ID && !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("snapshot cleanup checkpoint after applying %s: %v", preview.ID, err)
		}
	}
}

type snapshotPageSizeStore struct {
	storage.Store
	sizes map[string]int64
}

func (s snapshotPageSizeStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	entry, err := s.Store.Head(ctx, key)
	if size, ok := s.sizes[key]; ok && err == nil {
		entry.Size = size
	}
	return entry, err
}

func (s snapshotPageSizeStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	entries, next, err := storage.Scan(ctx, s.Store, prefix, token, limit)
	for i := range entries {
		if size, ok := s.sizes[entries[i].Key]; ok {
			entries[i].Size = size
		}
	}
	return entries, next, err
}

func completeHistoryRangePreview(t *testing.T, s *Service, request historyCleanupRangeRequest) historyCleanupRangePreview {
	t.Helper()
	ctx := context.Background()
	preview, err := s.createHistoryCleanupRangePreview(ctx, "operator", request)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.backgroundQueue().Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID))
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		_, done, err := s.historyCleanupRangePreviewStep(ctx, &job)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			result, err := s.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
	}
	t.Fatal("range preview did not finish")
	return historyCleanupRangePreview{}
}
