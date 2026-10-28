package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) inventoryStart(ctx context.Context, task *tasks.Task, digest string) error {
	if s.inventory == nil {
		return nil
	}
	if task.Kind == tasks.BlobKind {
		return s.inventoryStartBlob(ctx, task)
	}
	identity := inventoryIdentity(task.Authority, task.Repository, task.Reference)
	binding, _, err := inventory.Read[inventory.Binding](ctx, s.store, inventory.Prefix+"bindings/"+identity.ID()+".json")
	if errors.Is(err, storage.ErrNotFound) {
		binding, err = s.inventoryBinding(ctx, identity, task.RootDigest, "task", task.Generation, task.CreatedAt, false)
	}
	if err != nil {
		return temporary("inventory_lifecycle", err)
	}
	contentGeneration, generationErr := s.generation(ctx, task.Authority, task.Repository, task.RootDigest)
	if generationErr != nil {
		return temporary("inventory_lifecycle", generationErr)
	}
	if binding.Digest == task.RootDigest && binding.Generation == task.Generation && binding.ContentGeneration != contentGeneration.Generation {
		binding, err = s.inventoryBinding(ctx, identity, task.RootDigest, binding.Source, task.Generation, time.Now().UTC(), false)
		if err != nil {
			return temporary("inventory_lifecycle", err)
		}
	}
	if binding.Digest == task.RootDigest && binding.Generation == task.Generation {
		if err := s.inventory.Start(ctx, inventory.LifecycleVersion(identity.ID(), binding.Revision, binding.ContentGeneration)); err != nil {
			return temporary("inventory_lifecycle", err)
		}
	}
	for _, manifestDigest := range []string{task.RootDigest, digest} {
		generation, err := s.generation(ctx, task.Authority, task.Repository, manifestDigest)
		if err != nil {
			return temporary("inventory_lifecycle", err)
		}
		version := inventory.LifecycleVersion(inventory.ID("platform", task.Authority, task.Repository, manifestDigest), "", generation.Generation)
		if err := s.inventory.Start(ctx, version); err != nil {
			return temporary("inventory_lifecycle", err)
		}
		if err := s.inventoryStartDigestReference(ctx, task, manifestDigest, generation.Generation); err != nil {
			return temporary("inventory_lifecycle", err)
		}
	}
	return nil
}

func (s *Service) inventoryStartBlob(ctx context.Context, task *tasks.Task) error {
	token := ""
	for {
		demands, next, err := s.records.ListBlobDemandRecords(ctx, task.ID, token, 100)
		if err != nil {
			return temporary("inventory_lifecycle", err)
		}
		for _, demand := range demands {
			if demand.RootTaskID == "" {
				continue
			}
			root, err := s.tasks.Get(ctx, demand.RootTaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return temporary("inventory_lifecycle", err)
			}
			if root.Status == tasks.Failed || root.Status == tasks.Cancelled || root.Status == tasks.Solidified || root.CancelRequested {
				continue
			}
			if err := s.inventoryStart(ctx, &root, root.RootDigest); err != nil {
				return err
			}
			if err := s.inventoryStartDemand(ctx, root, demand.ScopeID); err != nil {
				return temporary("inventory_lifecycle", err)
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (s *Service) inventoryStartDemand(ctx context.Context, root tasks.Task, scope string) error {
	token := ""
	for {
		nodes, next, err := s.records.ListGraphNodes(ctx, root.ID, token, 100)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			if node.Role != tasks.GraphManifest {
				continue
			}
			id, err := protectionScopeID(root.Authority, root.Repository, node.Digest, root.Generation)
			if err != nil {
				return err
			}
			if id == scope {
				return s.inventoryStart(ctx, &root, node.Digest)
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}
