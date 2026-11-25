package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

func TestHistoryCleanupResumesEventDaySealFromOperationCheckpoint(t *testing.T) {
	ctx := context.Background()
	store := &eventSealFaultStore{Store: newFilesystemStore(t), sealReads: map[string]int{}, writes: map[string]int{}}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	start := time.Now().UTC().Truncate(24 * time.Hour).Add(-21 * 24 * time.Hour)
	end := start.Add(7 * 24 * time.Hour)
	firstDay := diagnostics.EventHistorySealPrefix + start.Format("20060102") + ".json"
	secondDay := start.Add(24 * time.Hour)
	event := diagnostics.Event{ID: "last-day", OccurredAt: end.Add(-time.Hour)}
	if err := putHistoryCleanupJSON(ctx, s.store, diagnostics.EventKey(event), event, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	preview := completeHistoryRangePreview(t, s, historyCleanupRangeRequest{Category: "diagnostic_events", From: start, To: end})
	task, status, err := s.submitHistoryCleanup(ctx, "operator", "seal-resume", historyCleanupExecutionRequest{PreviewID: preview.ID, SegmentID: preview.SegmentID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit status=%d err=%v", status, err)
	}
	queue := s.backgroundQueue()
	job, err := queue.Read(ctx, historyCleanupWorkKind, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := s.historyCleanupStep(ctx, &job); done || err != nil {
		t.Fatalf("first day done=%t err=%v", done, err)
	}
	reads := store.sealReads[firstDay]
	store.timeoutKey = diagnostics.EventHistorySealPrefix + secondDay.Format("20060102") + ".json"
	if _, done, err := s.historyCleanupStep(ctx, &job); done || err != nil {
		t.Fatalf("timed-out step done=%t err=%v", done, err)
	}
	operationKey := historyCleanupOperationKey(task.ID)
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, operationKey)
	if err != nil || !operation.EventSealedThrough.Equal(secondDay) || operation.SegmentOffset != 0 || store.sealReads[firstDay] != reads {
		t.Fatalf("checkpoint after timeout=%+v first-day reads=%d err=%v", operation, store.sealReads[firstDay], err)
	}
	if _, err := s.store.Head(ctx, diagnostics.EventKey(event)); err != nil {
		t.Fatalf("event disappeared before sealing finished: %v", err)
	}
	store.timeoutKey = ""
	for range 16 {
		// Read the original queue record each time to simulate loss of queue progress.
		job, err := queue.Read(ctx, historyCleanupWorkKind, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, done, err := s.historyCleanupStep(ctx, &job)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			break
		}
	}
	operation, _, err = inventory.Read[historyCleanupOperation](ctx, s.store, operationKey)
	if err != nil || operation.Status != "completed" || operation.Counts["diagnostic_events"].Deleted != 1 || store.sealReads[firstDay] != reads {
		t.Fatalf("resumed operation=%+v first-day reads=%d err=%v", operation, store.sealReads[firstDay], err)
	}
	if sealed, err := diagnostics.EventBehindWatermark(ctx, s.store, event); !sealed || err != nil {
		t.Fatalf("last day sealed=%t err=%v", sealed, err)
	}
}

func TestDiagnosticRetentionReusesEventSealAndSharesDayBackoff(t *testing.T) {
	ctx := context.Background()
	store := &eventSealFaultStore{Store: newFilesystemStore(t), sealReads: map[string]int{}, writes: map[string]int{}}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	at := time.Now().UTC().Truncate(24 * time.Hour).Add(-21*24*time.Hour + time.Hour)
	date := at.Format("20060102")
	seal := diagnostics.EventHistorySealPrefix + date + ".json"
	store.failKey = seal
	for _, id := range []string{"one", "two", "three"} {
		event := diagnostics.Event{ID: id, OccurredAt: at}
		if err := putHistoryCleanupJSON(ctx, s.store, diagnostics.EventKey(event), event, storage.PutOptions{IfAbsent: true}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := storage.Scan(ctx, s.store, diagnostics.Prefix+"events/"+date+"/", "", 8)
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	for i := range entries {
		entry, err := s.store.Head(ctx, entries[i].Key)
		if err != nil {
			t.Fatal(err)
		}
		entry.ModTime = at.UnixNano()
		entries[i] = entry
	}
	var checkpoint sourceSweepCheckpoint
	if err := s.cleanupDiagnosticRetentionEntry(ctx, entries[0], &checkpoint); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("first seal err=%v", err)
	}
	if _, err := s.store.Head(ctx, entries[0].Key); err != nil {
		t.Fatalf("event was removed before its day seal committed: %v", err)
	}
	var deferred diagnosticPartitionDeferred
	if err := s.cleanupDiagnosticRetentionEntry(ctx, entries[1], &checkpoint); !errors.As(err, &deferred) || store.writes[seal] != 1 {
		t.Fatalf("same-day attempt during backoff: writes=%d err=%v", store.writes[seal], err)
	}
	store.failKey = ""
	s.diagnosticSealBackoff.clear(date)
	for _, entry := range entries {
		if err := s.cleanupDiagnosticRetentionEntry(ctx, entry, &checkpoint); err != nil {
			t.Fatal(err)
		}
	}
	leaseKey := "leases/diagnostics/event-history-day/" + date
	if store.writes[seal] != 2 || store.writes[leaseKey] != 4 || checkpoint.Status.Counts["deleted"] != 3 {
		t.Fatalf("seal writes=%d lease writes=%d counters=%v", store.writes[seal], store.writes[leaseKey], checkpoint.Status.Counts)
	}
}

func TestDiagnosticRetentionDeletesSealedDayBeforeWatermarkRecovery(t *testing.T) {
	ctx := context.Background()
	base := newFilesystemStore(t)
	store := &diagnosticBatchReadFaultStore{Store: base}
	s := newStagedAcceptanceFixtureWithStore(t, 1<<20, store).service
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(-21 * 24 * time.Hour)
	adjacent := day.Add(-24 * time.Hour)
	gap := day.Add(-48 * time.Hour)
	older := day.Add(-72 * time.Hour)
	for _, date := range []time.Time{adjacent, older} {
		if err := diagnostics.SealEventHistoryDay(ctx, s.store, date, date.Add(24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	event := diagnostics.Event{ID: "expired-event", OccurredAt: day.Add(time.Hour)}
	key := diagnostics.EventKey(event)
	if err := putHistoryCleanupJSON(ctx, s.store, key, event, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	entry, err := s.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	entry.ModTime = event.OccurredAt.UnixNano()
	store.key, store.failAt = diagnostics.EventHistorySealPrefix+adjacent.Format("20060102")+".json", 1
	var cleanup sourceSweepCheckpoint
	if err := s.cleanupDiagnosticRetentionEntry(ctx, entry, &cleanup); err != nil || cleanup.Status.Counts["deleted"] != 1 {
		t.Fatalf("delete sealed day: counts=%v err=%v", cleanup.Status.Counts, err)
	}
	if _, err := s.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired event remains: %v", err)
	}
	if store.reads != 0 {
		t.Fatalf("deletion read the adjacent day %d times", store.reads)
	}
	blocked, err := diagnostics.EventBehindWatermark(ctx, s.store, event)
	if err != nil || !blocked {
		t.Fatalf("sealed day still accepts late events: blocked=%t err=%v", blocked, err)
	}
	step := s.cleanupDiagnosticRetentionEntry
	p := sourceSweepCheckpoint{Backoff: map[string]entityBackoff{}, Round: 1}
	s.sourceSweepBatch(ctx, diagnosticEventWatermarkWorker, diagnostics.EventHistorySealPrefix, &p, step)
	if store.reads != 1 || p.Status.Counts["failed"] != 1 {
		t.Fatalf("watermark scan did not record the fault: reads=%d counts=%v", store.reads, p.Status.Counts)
	}
	checkWatermark := func(from, before time.Time) {
		t.Helper()
		wm, _, err := inventory.Read[diagnostics.EventHistoryWatermark](ctx, s.store, diagnostics.EventHistoryWatermarkKey)
		if err != nil || wm.FromDate != from.Format("20060102") || wm.BeforeDate != before.Format("20060102") {
			t.Fatalf("watermark=%+v err=%v", wm, err)
		}
	}
	checkWatermark(older, gap)
	restart := newStagedAcceptanceFixtureWithStore(t, 1<<20, base).service
	retryEntries, _, err := storage.Scan(ctx, restart.store, sourceRetryPrefix+diagnosticEventWatermarkWorker+"/", "", 2)
	if err != nil || len(retryEntries) != 1 {
		t.Fatalf("durable watermark retries=%d err=%v", len(retryEntries), err)
	}
	retry, _, err := inventory.Read[sourceSweepRetry](ctx, restart.store, retryEntries[0].Key)
	if err != nil || retry.SourceKey != store.key {
		t.Fatalf("watermark retry=%+v err=%v", retry, err)
	}
	timer := time.NewTimer(time.Until(retry.NextAttemptAt))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	manager := lease.NewManager(restart.store)
	holder, err := manager.Acquire(ctx, "background/source/"+diagnosticEventWatermarkWorker, "test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, holder)
	var resumed sourceSweepCheckpoint
	processed, err := restart.dispatchDueSourceSweepRetry(ctx, holder, diagnosticEventWatermarkWorker, &resumed, restart.cleanupDiagnosticRetentionEntry)
	if err != nil || !processed {
		t.Fatalf("watermark retry processed=%t err=%v", processed, err)
	}
	if _, err := restart.store.Head(ctx, retryEntries[0].Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("completed watermark retry remains: %v", err)
	}
	checkWatermark(older, gap)
	if err := diagnostics.SealEventHistoryDay(ctx, s.store, gap, adjacent); err != nil {
		t.Fatal(err)
	}
	restart.sourceSweepBatch(ctx, diagnosticEventWatermarkWorker, diagnostics.EventHistorySealPrefix, &resumed, restart.cleanupDiagnosticRetentionEntry)
	checkWatermark(older, day.Add(24*time.Hour))
}

type eventSealFaultStore struct {
	storage.Store
	sealReads  map[string]int
	writes     map[string]int
	timeoutKey string
	failKey    string
}

func (s *eventSealFaultStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, diagnostics.EventHistorySealPrefix) {
		s.sealReads[key]++
	}
	if key == s.timeoutKey {
		<-ctx.Done()
		return storage.Object{}, ctx.Err()
	}
	return s.Store.Get(ctx, key)
}

func (s *eventSealFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.writes[key]++
	if key == s.failKey {
		return storage.Entry{}, storage.ErrTimeout
	}
	return s.Store.Put(ctx, key, body, size, options)
}
