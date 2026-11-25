package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestTerminalHistoryRangeBackfillsLegacyProjections(t *testing.T) {
	at := time.Now().UTC().Add(-72 * time.Hour)
	workID := workqueue.ID("notify", "legacy-history")
	work := terminalWorkHistoryRecord{
		SchemaVersion: 1, Kind: "notify", ID: workID, SourceKey: workqueue.ReceiptKey("notify", workID),
		SourceVersion: "legacy-version", Revision: "legacy-revision", TerminalAt: at,
		View: backgroundJobView{Job: workqueue.Job{ID: workID, Kind: "notify"}},
	}
	transferID := diagnostics.ID("client_upload", "legacy-transfer-history")
	transfer := terminalTransferHistoryRecord{
		SchemaVersion: 1, ID: transferID, SourceKey: diagnostics.Prefix + "transfers/" + transferID + ".json",
		SourceVersion: "legacy-version", Revision: "legacy-revision", TerminalAt: at,
		View: transferView{ID: transferID, Kind: "client_upload"},
	}
	for _, test := range []struct {
		category, worker, prefix, key, indexKey string
		record                                  any
	}{
		{"terminal_work_history", terminalWorkHistoryRetentionWorker, terminalWorkHistoryPrefix,
			terminalWorkHistoryKey(work.Kind, work.ID), historyOrderKey(terminalWorkHistoryOrderPrefix, at, work.Kind, work.ID, work.Revision), work},
		{"terminal_transfer_history", terminalTransferHistoryRetentionWorker, terminalTransferHistoryPrefix,
			terminalTransferHistoryKey(transfer.ID), historyOrderKey(terminalTransferHistoryOrderPrefix, at, transfer.View.Kind, transfer.ID, transfer.Revision), transfer},
	} {
		t.Run(test.category, func(t *testing.T) {
			s := newStagedAcceptanceFixture(t, 1<<20).service
			ctx := context.Background()
			if err := putHistoryCleanupJSON(ctx, s.store, test.key, test.record, storage.PutOptions{IfAbsent: true}); err != nil {
				t.Fatal(err)
			}
			prior := sourceSweepCheckpoint{
				Round: 12, Cursor: inventory.Cursor{Done: true, Entities: 7}, NextRound: time.Now().Add(time.Hour),
				Status: backgroundScanStatus{Complete: true, LastSuccessfulRound: 11, LastSuccessAt: time.Now()},
			}
			if err := putHistoryCleanupJSON(ctx, s.store, sourceSweepKey(test.worker), prior, storage.PutOptions{IfAbsent: true}); err != nil {
				t.Fatal(err)
			}
			preview, err := s.createHistoryCleanupRangePreview(ctx, "operator", historyCleanupRangeRequest{
				Category: test.category, From: at.Add(-time.Minute), To: at.Add(time.Minute),
			})
			if err != nil {
				t.Fatal(err)
			}
			queue := s.backgroundQueue()
			job, err := queue.Read(ctx, historyCleanupPreviewKind, workqueue.ID(historyCleanupPreviewKind, preview.ID))
			if err != nil {
				t.Fatal(err)
			}
			if _, done, err := s.historyCleanupRangePreviewStep(ctx, &job); err != nil || done {
				t.Fatalf("preview before backfill: done=%t error=%v", done, err)
			}
			waiting, err := s.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
			if err != nil || waiting.Status != "waiting_for_backfill" || waiting.Counts.Scanned != 0 ||
				len(waiting.BackfillPending) != 1 || waiting.BackfillPending[0] != test.worker {
				t.Fatalf("waiting preview = %+v, error = %v", waiting, err)
			}

			runTestTerminalHistoryBackfill(t, s, test.worker, test.prefix)
			index, err := s.store.Head(ctx, test.indexKey)
			if err != nil {
				t.Fatalf("repaired index: %v", err)
			}
			var automatic sourceSweepCheckpoint
			if err := s.cleanupDiagnosticRetentionEntry(ctx, index, &automatic); err != nil {
				t.Fatal(err)
			}
			if _, err := s.store.Head(ctx, test.indexKey); err != nil {
				t.Fatalf("automatic cleanup removed a linked index: %v", err)
			}

			if _, done, err := s.historyCleanupRangePreviewStep(ctx, &job); err != nil || !done {
				t.Fatalf("preview after backfill: done=%t error=%v", done, err)
			}
			completed, err := s.historyCleanupRangePreviewStatus(ctx, preview.ID, "operator")
			if err != nil || completed.Status != "completed" || completed.Counts.Eligible != 2 || len(completed.BackfillPending) != 0 {
				t.Fatalf("completed preview = %+v, error = %v", completed, err)
			}
			task, status, err := s.submitHistoryCleanup(ctx, "operator", "legacy-backfill",
				historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: completed.SegmentID})
			if err != nil || status != http.StatusAccepted {
				t.Fatalf("submit cleanup: status=%d error=%v", status, err)
			}
			job, err = queue.Read(ctx, historyCleanupWorkKind, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 4 {
				if _, done, err := s.historyCleanupStep(ctx, &job); err != nil {
					t.Fatal(err)
				} else if done {
					break
				}
			}
			for _, key := range []string{test.key, test.indexKey} {
				if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("%q remains after cleanup: %v", key, err)
				}
			}
		})
	}
}

func runTestTerminalHistoryBackfill(t *testing.T, s *Service, worker, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "background/source/"+worker, "test-backfill", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(context.Background(), holder)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.runSourceSweepStateLeader(ctx, holder, worker, prefix, s.sweepTerminalHistory, s.finishTerminalHistoryIndexBackfill(worker))
	}()
	defer func() {
		cancel()
		<-stopped
	}()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		checkpoint, _, err := inventory.Read[sourceSweepCheckpoint](ctx, s.store, sourceSweepKey(worker))
		if err == nil && checkpoint.HistoryIndexBackfillComplete {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("backfill checkpoint did not complete: %+v, error = %v", checkpoint, err)
		case <-stopped:
			t.Fatal("backfill worker stopped before completing the checkpoint")
		case <-ticker.C:
		}
	}
}

func TestTerminalHistoryBackfillChecksDurableRetries(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	p := sourceSweepCheckpoint{Cursor: inventory.Cursor{Done: true}}
	worker := terminalWorkHistoryRetentionWorker
	retryKey := sourceRetryPrefix + worker + "/pending.json"
	entry, err := putSourceSweepFixture(ctx, s.store, retryKey, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	finish := s.finishTerminalHistoryIndexBackfill(worker)
	if err := finish(ctx, &p); err != nil || p.HistoryIndexBackfillComplete {
		t.Fatalf("backfill completed with a durable retry: %+v, error = %v", p, err)
	}
	if err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
		t.Fatal(err)
	}
	if err := finish(ctx, &p); err != nil || !p.HistoryIndexBackfillComplete {
		t.Fatalf("settled backfill did not complete: %+v, error = %v", p, err)
	}
}

func TestTerminalHistoryCleanupKeepsIndexLinkedAfterPreview(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	at := time.Now().UTC().Add(-72 * time.Hour)
	id := diagnostics.ID("client_upload", "linked-after-preview")
	record := terminalTransferHistoryRecord{
		SchemaVersion: 1, ID: id, SourceKey: diagnostics.Prefix + "transfers/" + id + ".json",
		SourceVersion: "version", Revision: "revision", TerminalAt: at,
		View: transferView{ID: id, Kind: "client_upload"},
	}
	indexKey := historyOrderKey(terminalTransferHistoryOrderPrefix, at, record.View.Kind, id, record.Revision)
	if err := putHistoryOrderIndex(ctx, s.store, indexKey); err != nil {
		t.Fatal(err)
	}
	checkpoint := sourceSweepCheckpoint{HistoryIndexBackfillVersion: terminalHistoryIndexBackfillVersion, HistoryIndexBackfillComplete: true}
	if err := putHistoryCleanupJSON(ctx, s.store, sourceSweepKey(terminalTransferHistoryRetentionWorker), checkpoint, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	preview := completeHistoryRangePreview(t, s, historyCleanupRangeRequest{
		Category: "terminal_transfer_history", From: at.Add(-time.Minute), To: at.Add(time.Minute),
	})
	if preview.Counts.Eligible != 1 {
		t.Fatalf("orphan index preview = %+v", preview)
	}
	if err := putHistoryCleanupJSON(ctx, s.store, terminalTransferHistoryKey(id), record, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	task, status, err := s.submitHistoryCleanup(ctx, "operator", "linked-index",
		historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: preview.SegmentID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit cleanup: status=%d error=%v", status, err)
	}
	job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, done, err := s.historyCleanupStep(ctx, &job); err != nil {
			t.Fatal(err)
		} else if done {
			break
		}
	}
	op, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(task.ID))
	if err != nil || op.Status != "completed" || op.Counts[preview.Category].SkipReasons["version_changed"] != 1 {
		t.Fatalf("cleanup result = %+v, error = %v", op, err)
	}
	if _, err := s.store.Head(ctx, indexKey); err != nil {
		t.Fatalf("linked index was removed: %v", err)
	}
}
