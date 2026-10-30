package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) reuseCachedManifestProjection(ctx context.Context, identity inventory.Identity, digest string) (bool, error) {
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, digest)
	if err != nil || generation.Tombstone {
		return false, err
	}
	content, err := s.inventory.Content(ctx, identity, digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if content.Generation != generation.Generation || !content.ClosureKnown || content.Source != "manifest" {
		return false, nil
	}
	done, _, err := inventory.Read[bool](ctx, s.store, projectionReceipt(identity, digest, generation.Generation))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return done, err
}
