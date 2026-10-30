package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
)

type inventoryJournalFailureStore struct {
	storage.Store
	key        string
	failRead   bool
	failDelete bool
}

func (s *inventoryJournalFailureStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if s.failRead && key == s.key {
		return storage.Object{}, storage.ErrTimeout
	}
	return s.Store.Get(ctx, key)
}

func (s *inventoryJournalFailureStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if s.failDelete && key == s.key {
		return storage.ErrTimeout
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestInventoryDeltaJournalResumesPartialParallelReadsAndAcknowledgement(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	for index := range 8 {
		if err := f.service.recordInventoryChange(ctx, fmt.Sprintf("tasks/state/%d.json", index), "source-version"); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err := storage.Scan(ctx, f.service.store, inventory.Prefix+"changes/", "", 32)
	if err != nil || len(entries) != 8 {
		t.Fatalf("journal fixture entries=%d error=%v", len(entries), err)
	}
	fault := &inventoryJournalFailureStore{Store: f.service.store, key: entries[1].Key, failRead: true}
	f.service.store = fault
	p := inventoryDeltaProgress{}
	if err := f.service.captureInventoryChanges(ctx, &p); !errors.Is(err, storage.ErrTimeout) || p.Cursor.Offset != 0 || len(p.Changes) != 0 {
		t.Fatalf("failed read advanced buffered work: offset=%d changes=%d error=%v", p.Cursor.Offset, len(p.Changes), err)
	}
	fault.failRead = false
	for !p.Cursor.Done {
		if err := f.service.captureInventoryChanges(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.Changes) != 8 {
		t.Fatalf("read replay lost journal entries: %d", len(p.Changes))
	}
	fault.failDelete = true
	if err := f.service.acknowledgeInventoryChanges(ctx, &p); !errors.Is(err, storage.ErrTimeout) || p.Offset != 0 {
		t.Fatalf("failed acknowledgement advanced checkpoint: offset=%d error=%v", p.Offset, err)
	}
	if _, err := fault.Head(ctx, entries[0].Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("fixture did not exercise a partially committed acknowledgement: %v", err)
	}
	fault.failDelete = false
	for p.Offset < len(p.Changes) {
		if err := f.service.acknowledgeInventoryChanges(ctx, &p); err != nil {
			t.Fatal(err)
		}
	}
	entries, _, err = storage.Scan(ctx, fault, inventory.Prefix+"changes/", "", 32)
	if err != nil || len(entries) != 0 {
		t.Fatalf("acknowledgement replay did not retire captured sources: entries=%d error=%v", len(entries), err)
	}
}
