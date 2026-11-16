package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type inventoryFragmentValidationKey struct{}

func fullFragmentValidation(ctx context.Context) bool {
	full, _ := ctx.Value(inventoryFragmentValidationKey{}).(bool)
	return full
}

func (s *Service) allowFastInventoryFragment(ctx context.Context, p *inventoryImageProgress) (bool, error) {
	candidate := p.Reuse
	if fullFragmentValidation(ctx) || s.inventoryReconcile.Load() || candidate.Version == "" || time.Since(candidate.Fragment.CapturedAt) > min(s.inventoryConfig().SnapshotRetention, s.inventoryConfig().FragmentReuseInterval) {
		return false, nil
	}
	// A pending marker may not have reached dirty-image/content-fragment fanout yet.
	entries, _, err := storage.Scan(ctx, s.store, inventory.Prefix+"changes/", "", 1)
	if err != nil || len(entries) > 0 {
		return false, err
	}
	current, err := s.store.Head(ctx, inventoryFragmentKey(p.Image, candidate.Kind))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil || current.Version != candidate.Version {
		return false, err
	}
	if candidate.Kind == "image" {
		revision, err := s.inventoryImageDirtyRevision(ctx, p.Image.ID)
		if err != nil || revision != candidate.Fragment.DirtyRevision {
			return false, err
		}
	}
	return true, nil
}
