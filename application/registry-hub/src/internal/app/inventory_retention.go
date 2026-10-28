package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) cleanupInventorySnapshots(ctx context.Context, current string) error {
	if current == "" {
		snapshot, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
		if err == nil {
			current = snapshot.SnapshotID
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	token, _, err := inventory.Read[string](ctx, s.store, inventory.Prefix+"retention.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+"snapshots/", token, s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		id, _, ok := strings.Cut(strings.TrimPrefix(entry.Key, inventory.Prefix+"snapshots/"), "/")
		if !ok || id == current {
			continue
		}
		snapshot, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"snapshots/"+id+"/manifest.json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		expired := err == nil && !snapshot.ExpiresAt.After(time.Now())
		if errors.Is(err, storage.ErrNotFound) {
			expired = entry.ModTime != 0 && time.Since(time.Unix(0, entry.ModTime)) > s.inventoryConfig().SnapshotRetention
		}
		if expired {
			if err := s.store.Delete(ctx, entry.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
	}
	return inventory.Update(ctx, s.store, inventory.Prefix+"retention.json", func(value *string) error { *value = next; return nil })
}
