package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) cleanupInventorySnapshots(ctx context.Context, current string) (bool, error) {
	if current == "" {
		snapshot, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
		if err == nil {
			current = snapshot.SnapshotID
		} else if !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
	}
	token, _, err := inventory.Read[string](ctx, s.store, inventory.Prefix+"retention.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	checkpoint, _, err := inventory.Read[inventoryCheckpoint](ctx, s.store, inventory.Prefix+"checkpoint.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+"snapshots/", token, 1)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		id, _, ok := strings.Cut(strings.TrimPrefix(entry.Key, inventory.Prefix+"snapshots/"), "/")
		if !ok || id == current || checkpoint.Build != nil && id == checkpoint.Build.SnapshotID {
			continue
		}
		snapshot, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"snapshots/"+id+"/manifest.json")
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
		expired := err == nil && !snapshot.ExpiresAt.After(time.Now())
		if errors.Is(err, storage.ErrNotFound) {
			expired = entry.ModTime != 0 && time.Since(time.Unix(0, entry.ModTime)) > s.inventoryConfig().SnapshotRetention
		}
		if expired {
			if err := s.store.Delete(ctx, entry.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, err
			}
		}
	}
	err = inventory.Update(ctx, s.store, inventory.Prefix+"retention.json", func(value *string) error { *value = next; return nil })
	return next == "", err
}
