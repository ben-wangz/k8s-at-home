package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/workqueue"
)

func TestAutomaticHistoryBudgetRecoveryUsesDurableRetry(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	store := &historyBudgetWriteFaultStore{Store: base}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	at := time.Now().UTC().Add(-72 * time.Hour)
	id := diagnostics.ID("expired-work-projection")
	key := terminalWorkHistoryKey("notify", id)
	if err := putHistoryCleanupJSON(ctx, s.store, key, terminalWorkHistoryRecord{TerminalAt: at}, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	entry, err := s.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reserveHistoryProjection(ctx, terminalWorkHistoryPrefix, key, entry.Size, 10, 64<<10); err != nil {
		t.Fatal(err)
	}
	kind := terminalWorkHistoryRetentionWorker
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "background/source/"+kind, "budget-retry-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)
	p := sourceSweepCheckpoint{HistoryIndexBackfillComplete: true}
	if err := s.sweepTerminalHistory(ctx, entry, &p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Head(ctx, key); err != nil {
		t.Fatalf("source disappeared before retry dispatch: %v", err)
	}
	store.failBudgetWrite = true
	if handled, err := s.dispatchDueSourceSweepRetry(ctx, holder, kind, &p, s.sweepTerminalHistory); err != nil || !handled {
		t.Fatalf("dispatch with budget failure: handled=%t err=%v", handled, err)
	}
	if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("projection remains: %v", err)
	}
	budgetKey := diagnostics.Prefix + "history-budgets/" + inventory.ID(terminalWorkHistoryPrefix) + ".json"
	budget, _, err := inventory.Read[historyProjectionBudget](ctx, s.store, budgetKey)
	if err != nil || budget.TotalBytes != entry.Size {
		t.Fatalf("failed release did not leave the original charge: %+v, %v", budget, err)
	}
	id = sourceRetryID(kind, "default", key, sourceEntryIdentity(entry))
	retryKey := sourceRetryRecordKey(kind, id)
	retry, _, err := inventory.Read[sourceSweepRetry](ctx, s.store, retryKey)
	if err != nil || retry.SourceSize == nil || *retry.SourceSize != entry.Size {
		t.Fatalf("durable retry lost source metadata: %+v, %v", retry, err)
	}
	retry.NextAttemptAt = time.Now().UTC().Add(-time.Second)
	if err := inventory.Update(ctx, s.store, retryKey, func(current *sourceSweepRetry) error {
		*current = retry
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.publishSourceRetryDue(ctx, retry); err != nil {
		t.Fatal(err)
	}
	restarted := sourceSweepCheckpoint{}
	if handled, err := s.dispatchDueSourceSweepRetry(ctx, holder, kind, &restarted, s.sweepTerminalHistory); err != nil || !handled {
		t.Fatalf("dispatch after source loss: handled=%t err=%v", handled, err)
	}
	budget, _, err = inventory.Read[historyProjectionBudget](ctx, s.store, budgetKey)
	if err != nil || len(budget.Sizes) != 0 || budget.TotalBytes != 0 {
		t.Fatalf("retry left a capacity charge: %+v, %v", budget, err)
	}
	if _, err := s.store.Head(ctx, retryKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("settled retry remains: %v", err)
	}
}

func TestHistoryCleanupRangeCapacityIncludesLaterPages(t *testing.T) {
	s := newStagedAcceptanceFixture(t, 1<<20).service
	ctx := context.Background()
	now := time.Now().UTC()
	for i := range 2*historyCleanupMaxPreviews + 1 + historyCleanupMaxPreviews {
		expiry := now.Add(-time.Hour)
		if i >= 2*historyCleanupMaxPreviews+1 {
			expiry = now.Add(time.Hour)
		}
		id := fmt.Sprintf("%064x", i+1)
		record := historyCleanupRangePreview{ID: id, Status: "completed", ExpiresAt: expiry}
		if err := putHistoryCleanupJSON(ctx, s.store, historyCleanupRangePreviewKey(id), record, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.createHistoryCleanupRangePreview(ctx, "operator", historyCleanupRangeRequest{
		Category: "diagnostic_events", From: now.Add(-time.Hour), To: now,
	})
	if !errors.Is(err, errHistoryCleanupCapacity) {
		t.Fatalf("create with 100 live previews behind expired records: %v", err)
	}
	if err := s.store.Delete(ctx, historyCleanupRangePreviewKey(fmt.Sprintf("%064x", 3*historyCleanupMaxPreviews+1))); err != nil {
		t.Fatal(err)
	}
	if err := s.checkHistoryCleanupRangeCapacity(ctx); err != nil {
		t.Fatalf("expired records consumed the freed slot: %v", err)
	}
}

func TestHistoryCleanupKeepsConfirmedProjectionDeleteWhenIndexTimesOut(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	store := &historyCleanupIndexDeleteFaultStore{Store: base}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	at := time.Now().UTC().Add(-72 * time.Hour)
	id := workqueue.ID("notify", "index-delete-timeout")
	record := terminalWorkHistoryRecord{
		SchemaVersion: 1, Kind: "notify", ID: id, SourceVersion: "source", TerminalAt: at, Revision: "test",
		View: backgroundJobView{Job: workqueue.Job{Kind: "notify", ID: id}},
	}
	key := terminalWorkHistoryKey(record.Kind, id)
	if err := putHistoryCleanupJSON(ctx, s.store, key, record, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	projection, err := s.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	indexKey := historyOrderKey(terminalWorkHistoryOrderPrefix, at, record.Kind, id, record.Revision)
	if err := putHistoryOrderIndex(ctx, s.store, indexKey); err != nil {
		t.Fatal(err)
	}
	checkpoint := sourceSweepCheckpoint{HistoryIndexBackfillVersion: terminalHistoryIndexBackfillVersion, HistoryIndexBackfillComplete: true}
	if err := putHistoryCleanupJSON(ctx, s.store, sourceSweepKey(terminalWorkHistoryRetentionWorker), checkpoint, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	preview := completeHistoryRangePreview(t, s, historyCleanupRangeRequest{
		Category: "terminal_work_history", From: at.Add(-time.Minute), To: at.Add(time.Minute),
	})
	task, status, err := s.submitHistoryCleanup(ctx, "operator", "index-timeout",
		historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: preview.SegmentID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit: status=%d err=%v", status, err)
	}
	queue := s.backgroundQueue()
	job, err := queue.Read(ctx, historyCleanupWorkKind, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.failKey = indexKey
	if _, _, err := s.historyCleanupStep(ctx, &job); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("cleanup did not reach index timeout: %v", err)
	}
	operationKey := historyCleanupOperationKey(task.ID)
	op, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, operationKey)
	counts := op.Counts[preview.Category]
	if err != nil || !op.SegmentPrimarySettled || counts.Deleted != 1 || counts.DeletedBytes != projection.Size || counts.Unconfirmed != 0 {
		t.Fatalf("confirmed delete not checkpointed: %+v, %v", op, err)
	}
	job, err = queue.Read(ctx, historyCleanupWorkKind, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := s.historyCleanupStep(ctx, &job); err != nil || !done {
		t.Fatalf("resume with independent job read: done=%t err=%v", done, err)
	}
	op, _, err = inventory.Read[historyCleanupOperation](ctx, s.store, operationKey)
	counts = op.Counts[preview.Category]
	if err != nil || op.Status != "completed" || counts.Deleted != 2 || counts.Eligible != 1 || counts.Unconfirmed != 0 {
		t.Fatalf("final count = %+v, operation=%s err=%v", counts, op.Status, err)
	}
}

type historyCleanupIndexDeleteFaultStore struct {
	storage.Store
	failKey string
}

func (s *historyCleanupIndexDeleteFaultStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if key == s.failKey {
		s.failKey = ""
		return storage.ErrTimeout
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}
