package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) saveInventoryWork(ctx context.Context, key string, work inventoryWork) error {
	if err := inventory.Update(ctx, s.store, key, func(value *inventoryWork) error {
		if !value.UpdatedAt.After(work.UpdatedAt) {
			*value = work
		}
		return nil
	}); err != nil {
		return err
	}
	scope := key[:strings.LastIndex(key, "/")]
	return inventory.Update(ctx, s.store, inventory.Prefix+"latest-work/"+inventory.ID("work-scope", scope)+".json", func(value *inventoryWork) error {
		if !value.CreatedAt.After(work.CreatedAt) {
			*value = work
		}
		return nil
	})
}

type inventoryWorkRetention struct {
	Phase int    `json:"phase"`
	Token string `json:"token"`
}

func (s *Service) cleanupInventoryWork(ctx context.Context) (bool, error) {
	state, _, err := inventory.Read[inventoryWorkRetention](ctx, s.store, inventory.Prefix+"work-retention.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	prefixes := []string{"work/", "content-work/"}
	if state.Phase >= len(prefixes) {
		state.Phase = 0
		state.Token = ""
	}
	entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+prefixes[state.Phase], state.Token, 1)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if err := s.pruneInventoryWork(ctx, entry.Key); err != nil {
			return false, err
		}
	}
	state.Token = next
	if next == "" {
		state.Phase = (state.Phase + 1) % len(prefixes)
	}
	err = inventory.Update(ctx, s.store, inventory.Prefix+"work-retention.json", func(value *inventoryWorkRetention) error { *value = state; return nil })
	return state.Phase == 0 && next == "", err
}

func (s *Service) pruneInventoryWork(ctx context.Context, key string) error {
	work, version, err := inventory.Read[inventoryWork](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if inventoryWorkActive(work.Work) || time.Since(work.UpdatedAt) < 30*24*time.Hour {
		return nil
	}
	scope := key[:strings.LastIndex(key, "/")]
	latest, _, err := inventory.Read[inventoryWork](ctx, s.store, inventory.Prefix+"latest-work/"+inventory.ID("work-scope", scope)+".json")
	if err != nil {
		return err
	}
	if !latest.CreatedAt.After(work.CreatedAt) {
		return nil
	}
	_, err = s.tasks.Get(ctx, work.Work.ID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	err = s.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
