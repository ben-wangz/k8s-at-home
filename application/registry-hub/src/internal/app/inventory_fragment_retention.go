package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) cleanupInventoryFragments(ctx context.Context) (bool, error) {
	key := inventory.Prefix + "fragment-retention.json"
	cursor, _, err := inventory.Read[inventory.Cursor](ctx, s.store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	entry, ok, err := cursor.Peek(ctx, s.store, inventory.Prefix+"fragments/", 32)
	if err != nil {
		return false, err
	}
	if ok {
		value, version, err := inventory.Read[inventoryFragment](ctx, s.store, entry.Key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
		if err == nil && time.Since(value.CapturedAt) > s.inventoryConfig().SnapshotRetention {
			err = s.store.DeleteIfMatch(ctx, entry.Key, version)
			if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
				return false, err
			}
		}
		cursor.Advance()
	}
	done := cursor.Done
	if done {
		cursor = inventory.Cursor{}
	}
	err = inventory.Update(ctx, s.store, key, func(value *inventory.Cursor) error { *value = cursor; return nil })
	return done, err
}
