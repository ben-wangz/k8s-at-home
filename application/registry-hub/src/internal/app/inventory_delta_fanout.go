package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sync"
)

func (s *Service) invalidateInventoryFragments(ctx context.Context, p *inventoryDeltaProgress) error {
	if !p.Fanout.Done {
		done, err := s.invalidateInventoryFragmentBatch(ctx, p, &p.Fanout, "image")
		if err != nil || !done {
			return err
		}
	}
	if !p.ContentFanout.Done {
		_, err := s.invalidateInventoryContentFragments(ctx, p)
		return err
	}
	return nil
}

func (s *Service) invalidateInventoryContentFragments(ctx context.Context, p *inventoryDeltaProgress) (bool, error) {
	if p.ContentFanout.Done {
		return true, nil
	}
	return s.invalidateInventoryFragmentBatch(ctx, p, &p.ContentFanout, "content")
}

func (s *Service) invalidateInventoryFragmentBatch(ctx context.Context, p *inventoryDeltaProgress, cursor *inventory.Cursor, kind string) (bool, error) {
	_, ok, err := cursor.Peek(ctx, s.store, inventory.Prefix+"fragments/"+kind+"/", 32)
	if err != nil || !ok {
		return cursor.Done, err
	}
	count := min(4, len(cursor.Entries)-cursor.Offset)
	failures := make([]error, count)
	var workers sync.WaitGroup
	for index := range count {
		workers.Go(func() {
			fragment, _, err := inventory.Read[inventoryFragment](ctx, s.store, cursor.Entries[cursor.Offset+index].Key)
			if errors.Is(err, storage.ErrNotFound) {
				return
			}
			if err == nil {
				if kind == "content" {
					err = s.invalidateInventoryContentFragment(ctx, fragment, p.Changes, p.Fence)
				} else {
					err = s.invalidateInventoryFragment(ctx, fragment, p.Changes, p.Fence)
				}
			}
			failures[index] = err
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return false, err
	}
	for range count {
		cursor.Advance()
	}
	return cursor.Done, nil
}
