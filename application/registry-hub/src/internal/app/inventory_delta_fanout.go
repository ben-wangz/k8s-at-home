package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sync"
)

func (s *Service) invalidateInventoryFragments(ctx context.Context, p *inventoryDeltaProgress) error {
	_, ok, err := p.Fanout.Peek(ctx, s.store, inventory.Prefix+"fragments/image/", 32)
	if err != nil || !ok {
		return err
	}
	count := min(4, len(p.Fanout.Entries)-p.Fanout.Offset)
	failures := make([]error, count)
	var workers sync.WaitGroup
	for index := range count {
		workers.Go(func() {
			fragment, _, err := inventory.Read[inventoryFragment](ctx, s.store, p.Fanout.Entries[p.Fanout.Offset+index].Key)
			if errors.Is(err, storage.ErrNotFound) {
				return
			}
			if err == nil {
				err = s.invalidateInventoryFragment(ctx, fragment, p.Changes, p.Fence)
			}
			failures[index] = err
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	for range count {
		p.Fanout.Advance()
	}
	return nil
}
