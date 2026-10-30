package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type inventoryChange struct {
	Key           string    `json:"key"`
	SourceVersion string    `json:"sourceVersion,omitempty"`
	Revision      string    `json:"revision"`
	ChangedAt     time.Time `json:"changedAt"`
}

func inventoryChangeKey(key string) string {
	return inventory.Prefix + "changes/" + inventory.ID("source", key) + ".json"
}

func (s *observedStore) inventoryChanged(ctx context.Context, key, version string, err error) {
	if err != nil && !errors.Is(err, storage.ErrUnknownCommit) || !inventoryDependencyKey(key) && !inventorySourceKey(key) {
		return
	}
	if s.inventoryNotify != nil {
		s.inventoryNotify()
	}
	if s.inventoryChange != nil {
		if err := s.inventoryChange(ctx, key, version); err != nil {
			if s.metrics != nil {
				s.metrics.recordEvent("inventory_change_marker_failed")
			}
			if s.logger != nil {
				s.logger.Warn("inventory change requires reconciliation", "error", err)
			}
		}
	}
}

func (s *Service) recordInventoryChange(ctx context.Context, key, version string) error {
	if strings.HasPrefix(key, "solidification/graphs/") {
		root, _, ok := strings.Cut(strings.TrimPrefix(key, "solidification/graphs/"), "/")
		if ok && root != "" {
			key, version = "solidification/graphs/"+root+"/", ""
		}
	}
	// Marker failure cannot change the outcome of an already committed source write.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := inventory.Update(ctx, s.store, inventoryChangeKey(key), func(value *inventoryChange) error {
		value.Key, value.Revision, value.ChangedAt = key, requestID(), time.Now().UTC()
		value.SourceVersion = version
		return nil
	})
	if err != nil {
		s.inventoryReconcile.Store(true)
	}
	return err
}

func (s *Service) acknowledgeInventoryChange(ctx context.Context, change inventoryChange) error {
	key := inventoryChangeKey(change.Key)
	value, version, err := inventory.Read[inventoryChange](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) || err == nil && value.Revision != change.Revision {
		return nil
	}
	if err != nil {
		return err
	}
	err = s.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}
