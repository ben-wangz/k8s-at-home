package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strconv"
	"time"
)

func projectionReceipt(identity inventory.Identity, digest string, generation uint64) string {
	return inventory.Prefix + "projected/" + inventory.ID(identity.Authority, identity.Repository, digest, strconv.FormatUint(generation, 10)) + ".json"
}

func (s *Service) markManifestProjected(ctx context.Context, identity inventory.Identity, digest string, generation uint64) error {
	return inventory.Update(ctx, s.store, projectionReceipt(identity, digest, generation), func(done *bool) error { *done = true; return nil })
}

func (s *Service) retainUnprojectedManifest(ctx context.Context, authority, repository, digest, mediaType string, verifiedAt time.Time) (bool, error) {
	if s.inventory == nil || !inventoryManifestType(mediaType) {
		return false, nil
	}
	generation, err := s.generation(ctx, authority, repository, digest)
	if err != nil {
		return false, err
	}
	if generation.Tombstone || verifiedAt.Before(generation.UpdatedAt) {
		return false, nil
	}
	done, _, err := inventory.Read[bool](ctx, s.store, projectionReceipt(inventoryIdentity(authority, repository, digest), digest, generation.Generation))
	if errors.Is(err, storage.ErrNotFound) {
		return true, nil
	}
	return !done, err
}
