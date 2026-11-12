package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"testing"
)

type executionReadFailureStore struct {
	storage.Store
	err error
}

func (s executionReadFailureStore) Get(context.Context, string) (storage.Object, error) {
	return storage.Object{}, s.err
}

func TestExecutionProjectionHandlesSourcesRemovedAfterListing(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, scope := range []bool{false, true} {
		for _, cause := range []error{storage.ErrNotFound, storage.ErrUnavailable} {
			s := &Service{store: executionReadFailureStore{Store: store, err: cause}}
			cursor := inventory.Cursor{Entries: []storage.Entry{{Key: "removed-after-page-read"}}}
			p := inventoryExecutionProgress{Cursor: cursor}
			if scope {
				p = inventoryExecutionProgress{Nodes: cursor, Demand: &tasks.BlobDemand{}}
			}
			var progress any
			if scope {
				progress, _, err = s.projectBlobExecutionScope(context.Background(), &workqueue.Job{}, tasks.Task{}, p)
			} else {
				progress, _, err = s.projectBlobExecutionStep(context.Background(), &workqueue.Job{}, inventoryExecutionFact{}, p)
			}
			if cause != storage.ErrNotFound {
				if !errors.Is(err, cause) {
					t.Fatalf("scope=%t: storage failure was hidden: %v", scope, err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("scope=%t: removed source caused an endless retry: %v", scope, err)
			}
			updated := progress.(inventoryExecutionProgress)
			advanced := updated.Cursor.Done
			if scope {
				advanced = updated.Nodes.Done
			}
			if !advanced {
				t.Fatalf("scope=%t: projection did not advance past the removed source", scope)
			}
		}
	}
}
