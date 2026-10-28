package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) scanInventoryObservations(ctx context.Context, state *inventoryScan) error {
	entries, next, err := storage.Scan(ctx, s.store, "solidification/observations/", state.Token, s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		observation, _, err := inventory.Read[tasks.LocalObservation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if observation.SchemaVersion != 1 || observation.Role != tasks.GraphManifest || artifactDigest(observation.Digest) != nil {
			continue
		}
		identity := inventoryIdentity(observation.Authority, observation.Repository, observation.Digest)
		generation, err := s.generation(ctx, identity.Authority, identity.Repository, observation.Digest)
		if err != nil {
			return err
		}
		if generation.Tombstone || generation.Generation != observation.Generation {
			continue
		}
		_, err = s.inventory.Content(ctx, identity, observation.Digest)
		if errors.Is(err, storage.ErrNotFound) {
			content := inventory.Content{Identity: inventoryIdentity(identity.Authority, identity.Repository, ""), ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: observation.Digest, Size: observation.ActualSize}, Generation: observation.Generation, UpdatedAt: observation.ObservedAt}
			if err := s.inventory.SaveContent(ctx, content); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := s.inventoryBinding(ctx, identity, observation.Digest, "observation", observation.Generation, observation.ObservedAt, false); err != nil {
			return err
		}
	}
	state.Token = next
	if next == "" {
		state.Phase++
	}
	return nil
}
