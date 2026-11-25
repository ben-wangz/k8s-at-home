package diagnostics

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestEventHistoryWatermarkRejectsDelayedEventsAndOnlyAdvances(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := AdvanceEventHistoryWatermark(ctx, store, "20261120"); err != nil {
		t.Fatal(err)
	}
	if err := AdvanceEventHistoryWatermark(ctx, store, "20261118"); err != nil {
		t.Fatal(err)
	}
	old := Event{ID: "late", OccurredAt: time.Date(2026, 11, 19, 23, 59, 0, 0, time.UTC)}
	behind, err := EventBehindWatermark(ctx, store, old)
	if err != nil || !behind {
		t.Fatalf("old event behind watermark = %t, %v", behind, err)
	}
	if _, err := (BatchLog{Store: store}).Append(ctx, []Event{old}, false); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("late event append error = %v", err)
	}
	if err := (Log{Store: store}).Append(ctx, old); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("late legacy event append error = %v", err)
	}
	if _, err := store.Head(ctx, EventKey(old)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("late legacy event was persisted: %v", err)
	}
	current := Event{ID: "current", OccurredAt: time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)}
	behind, err = EventBehindWatermark(ctx, store, current)
	if err != nil || behind {
		t.Fatalf("event on watermark date = %t, %v", behind, err)
	}
	if err := AdvanceEventHistoryWatermark(ctx, store, "invalid"); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("invalid watermark error = %v", err)
	}
}

func TestLegacyEventAppendRechecksWatermarkAfterCommit(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	event := Event{ID: "watermark-race", OccurredAt: time.Now().UTC().Add(-12 * time.Hour)}
	date := event.OccurredAt.UTC().Format("20060102")
	storeWithWatermarkRace := &watermarkAdvancingStore{Store: store, afterEventPut: func() {
		if err := AdvanceEventHistoryWatermark(ctx, store, event.OccurredAt.UTC().Add(24*time.Hour).Format("20060102")); err != nil {
			t.Errorf("advance watermark after event commit: %v", err)
		}
	}}
	if err := (Log{Store: storeWithWatermarkRace}).Append(ctx, event); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("event racing watermark append error = %v", err)
	}
	if _, err := store.Head(ctx, EventKey(event)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("event behind committed watermark remains: %v", err)
	}
	watermark, _, err := inventory.Read[EventHistoryWatermark](ctx, store, EventHistoryWatermarkKey)
	if err != nil || watermark.BeforeDate <= date {
		t.Fatalf("watermark=%+v error=%v; expected it to pass event date %s", watermark, err, date)
	}
}

func TestSealedEventRangesAdvanceOnlyAcrossAdjacentFullDays(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	day18 := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	day19 := day18.Add(24 * time.Hour)
	day20 := day19.Add(24 * time.Hour)
	day21 := day20.Add(24 * time.Hour)
	if err := SealEventHistoryRange(ctx, store, day20, day21); err != nil {
		t.Fatal(err)
	}
	if err := SealEventHistoryRange(ctx, store, day18, day19); err != nil {
		t.Fatal(err)
	}
	watermark, _, err := inventory.Read[EventHistoryWatermark](ctx, store, EventHistoryWatermarkKey)
	if err != nil || watermark.FromDate != "20261120" || watermark.BeforeDate != "20261121" {
		t.Fatalf("disconnected seals advanced watermark=%+v error=%v", watermark, err)
	}
	if err := SealEventHistoryRange(ctx, store, day19, day20); err != nil {
		t.Fatal(err)
	}
	watermark, _, err = inventory.Read[EventHistoryWatermark](ctx, store, EventHistoryWatermarkKey)
	if err != nil || watermark.FromDate != "20261118" || watermark.BeforeDate != "20261121" {
		t.Fatalf("adjacent seals failed to advance watermark=%+v error=%v", watermark, err)
	}
	for _, test := range []struct {
		at      time.Time
		behind  bool
		message string
	}{
		{at: day18.Add(time.Hour), behind: true, message: "sealed day"},
		{at: day21.Add(time.Hour), behind: false, message: "day outside sealed prefix"},
	} {
		behind, err := EventBehindWatermark(ctx, store, Event{ID: "watermark-test", OccurredAt: test.at})
		if err != nil || behind != test.behind {
			t.Fatalf("%s watermark result=%t error=%v", test.message, behind, err)
		}
	}
}

func TestBatchAppendAndRangeSealSerializeWithoutDroppingOutOfRangeEvents(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	from := time.Now().UTC().Add(-12 * time.Hour)
	inside := Event{ID: "batch-seal-race-inside", OccurredAt: from}
	outside := Event{ID: "batch-seal-race-outside", OccurredAt: from.Add(time.Hour)}
	blocked := &batchPutBlockingStore{Store: store, entered: make(chan struct{}), release: make(chan struct{})}
	appendResult := make(chan error, 1)
	go func() {
		_, appendErr := (BatchLog{Store: blocked}).Append(ctx, []Event{inside, outside}, false)
		appendResult <- appendErr
	}()
	<-blocked.entered
	if err := SealEventHistoryRange(ctx, store, from, from.Add(30*time.Minute)); !errors.Is(err, lease.ErrHeld) {
		t.Fatalf("concurrent range seal error = %v, want held date lease", err)
	}
	close(blocked.release)
	if err := <-appendResult; err != nil {
		t.Fatalf("append before range seal: %v", err)
	}
	if err := SealEventHistoryRange(ctx, store, from, from.Add(30*time.Minute)); err != nil {
		t.Fatalf("seal range after append: %v", err)
	}
	keyPrefix := BatchPrefix + EventBatchDate(inside) + "/"
	entries, _, err := storage.Scan(ctx, store, keyPrefix, "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("mixed batch was removed: entries=%d error=%v", len(entries), err)
	}
	batch, _, err := inventory.Read[EventBatch](ctx, store, entries[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rejectBatchIfBehindWatermark(ctx, store, batch, entries[0]); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("post-commit seal check error = %v, want expired history", err)
	}
	if _, err := (BatchLog{Store: store}).Append(ctx, []Event{inside, outside}, false); !errors.Is(err, ErrBatchHistoryExpired) {
		t.Fatalf("append into sealed range = %v, want expired history", err)
	}
	if _, err := store.Head(ctx, entries[0].Key); err != nil {
		t.Fatalf("existing mixed batch was removed by rejected retry: %v", err)
	}
}

type watermarkAdvancingStore struct {
	storage.Store
	afterEventPut func()
}

type batchPutBlockingStore struct {
	storage.Store
	entered chan struct{}
	release chan struct{}
}

func (store *batchPutBlockingStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := store.Store.Put(ctx, key, reader, size, options)
	if err == nil && strings.HasPrefix(key, BatchPrefix) {
		close(store.entered)
		<-store.release
	}
	return entry, err
}

func (store *watermarkAdvancingStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := store.Store.Put(ctx, key, reader, size, options)
	if err == nil && strings.HasPrefix(key, Prefix+"events/") {
		store.afterEventPut()
	}
	return entry, err
}
