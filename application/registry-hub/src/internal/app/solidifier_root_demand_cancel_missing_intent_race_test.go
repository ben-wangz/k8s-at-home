package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

func TestRootDemandCleanupIgnoresIntentDeletedBeforeCancellationUpdate(t *testing.T) {
	ctx := context.Background()
	baseStore, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer baseStore.Close()

	intent, _, err := tasks.NewRecords(baseStore).EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority:  "registry.example",
		Repository: "team/image",
		Digest:     "sha256:" + strings.Repeat("a", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	rootID, demandID, sessionID := strings.Repeat("b", 32), strings.Repeat("c", 32), strings.Repeat("d", 32)
	if err := tasks.NewRecords(baseStore).LinkBlobDemandScope(ctx, intent.ID, demandID, rootID, "scope-1", sessionID, 64); err != nil {
		t.Fatal(err)
	}
	store := &deleteIntentBeforeTaskReadStore{Store: baseStore, intentID: intent.ID}
	records := tasks.NewRecords(store)
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	processor := &solidifier{service: &Service{records: records, gate: gate}, tasks: manager, gate: gate}

	if reason, err := processor.revokeRootBlobDemands(ctx, rootID); err != nil {
		t.Fatalf("revokeRootBlobDemands() reason=%q error=%v", reason, err)
	}
	if !store.deleted {
		t.Fatal("test did not delete the intent between its read and cancellation update")
	}
	demands, next, err := records.ListRootBlobDemandRecords(ctx, rootID, "", 100)
	if err != nil || next != "" || len(demands) != 0 {
		t.Fatalf("remaining root demands = %#v next=%q error=%v", demands, next, err)
	}
	if _, err := records.GetBlobIntent(ctx, intent.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetBlobIntent() error = %v, want storage.ErrNotFound", err)
	}
}

type deleteIntentBeforeTaskReadStore struct {
	storage.Store
	intentID string
	deleted  bool
}

func (s *deleteIntentBeforeTaskReadStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == "tasks/state/"+s.intentID+".json" && !s.deleted {
		s.deleted = true
		entry, err := s.Store.Head(ctx, "solidification/blobs/intents/"+s.intentID+".json")
		if err != nil {
			return storage.Object{}, err
		}
		if err := s.Store.DeleteIfMatch(ctx, "solidification/blobs/intents/"+s.intentID+".json", entry.Version); err != nil {
			return storage.Object{}, err
		}
	}
	return s.Store.Get(ctx, key)
}
