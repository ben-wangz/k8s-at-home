package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) captureInventoryProjectionChanges(ctx context.Context, p *inventoryGraphProjection) error {
	keys := []string{"tasks/state/" + p.Task.ID + ".json", "solidification/graphs/" + p.Task.ID + "/"}
	for _, key := range keys {
		change, _, err := inventory.Read[inventoryChange](ctx, s.store, inventoryChangeKey(key))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if change.Key == key && change.Revision != "" && (key == keys[1] || change.SourceVersion == p.Version) {
			p.Changes = append(p.Changes, change)
		}
	}
	return nil
}

func (s *Service) acknowledgeInventoryProjection(ctx context.Context, p *inventoryGraphProjection) error {
	for _, change := range p.Changes {
		if err := s.acknowledgeInventoryChange(ctx, change); err != nil {
			return err
		}
	}
	return nil
}
