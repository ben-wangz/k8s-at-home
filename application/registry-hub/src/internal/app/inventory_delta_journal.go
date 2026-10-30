package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"sync"
)

func (s *Service) captureInventoryChanges(ctx context.Context, p *inventoryDeltaProgress) error {
	_, ok, err := p.Cursor.Peek(ctx, s.store, inventory.Prefix+"changes/", 32)
	if err != nil || !ok {
		return err
	}
	count := min(4, 128-len(p.Changes), len(p.Cursor.Entries)-p.Cursor.Offset)
	changes, failures := make([]inventoryChange, count), make([]error, count)
	var workers sync.WaitGroup
	for index := range count {
		workers.Go(func() {
			value, _, err := inventory.Read[inventoryChange](ctx, s.store, p.Cursor.Entries[p.Cursor.Offset+index].Key)
			if errors.Is(err, storage.ErrNotFound) {
				err = nil
			}
			changes[index], failures[index] = value, err
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	for _, change := range changes {
		if change.Key != "" && change.Revision != "" {
			p.Changes = append(p.Changes, change)
		}
		p.Cursor.Advance()
	}
	return nil
}

func (s *Service) acknowledgeInventoryChanges(ctx context.Context, p *inventoryDeltaProgress) error {
	end := min(p.Offset+4, len(p.Changes))
	failures := make([]error, end-p.Offset)
	var workers sync.WaitGroup
	for index := range failures {
		workers.Go(func() {
			change := p.Changes[p.Offset+index]
			if !p.PublicationsOnly || strings.HasPrefix(change.Key, "distribution/repositories/") {
				failures[index] = s.acknowledgeInventoryChange(ctx, change)
			}
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	p.Offset = end
	return nil
}
