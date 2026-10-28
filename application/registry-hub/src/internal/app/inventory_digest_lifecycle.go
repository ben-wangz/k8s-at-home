package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) inventoryStartDigestReference(ctx context.Context, task *tasks.Task, digest string, generation uint64) error {
	if digest == "" {
		return nil
	}
	identity := inventoryIdentity(task.Authority, task.Repository, digest)
	binding, err := s.inventoryBinding(ctx, identity, digest, "execution", generation, time.Now().UTC(), false)
	if err != nil {
		return err
	}
	return s.inventory.Start(ctx, inventory.LifecycleVersion(identity.ID(), binding.Revision, binding.ContentGeneration))
}
