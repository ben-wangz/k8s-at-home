package diagnostics

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/inventory"
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

type watermarkAdvancingStore struct {
	storage.Store
	afterEventPut func()
}

func (store *watermarkAdvancingStore) Put(ctx context.Context, key string, reader io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := store.Store.Put(ctx, key, reader, size, options)
	if err == nil && strings.HasPrefix(key, Prefix+"events/") {
		store.afterEventPut()
	}
	return entry, err
}
