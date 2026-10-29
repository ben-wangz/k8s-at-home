package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) buildInventoryPage(ctx context.Context, state *inventoryCheckpoint) error {
	if state.BuildPhase < 0 || state.BuildPhase > 5 {
		return storage.ErrCorruptObject
	}
	writer := s.inventory.ResumeWriter(*state.Build)
	if state.BuildPhase == 5 {
		return s.publishInventory(ctx, state, writer)
	}
	if state.BuildPhase == 4 {
		done, err := writer.Assemble(ctx, s.inventoryConfig().BatchSize)
		if err != nil {
			return err
		}
		if done {
			state.BuildPhase++
		}
		state.Build = &writer.Snapshot
		return nil
	}
	if state.BuildPhase == 3 {
		done, next, err := s.completeInventoryPlatforms(ctx, writer, state.BuildToken)
		if err != nil {
			return err
		}
		if done {
			state.BuildPhase++
		}
		state.BuildToken = next
		return nil
	}
	prefixes := []string{"contents/", "bindings/", "cache/"}
	entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+prefixes[state.BuildPhase], state.BuildToken, s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		switch state.BuildPhase {
		case 0:
			err = s.buildInventoryContent(ctx, writer, entry.Key)
		case 1:
			err = s.buildInventoryReference(ctx, writer, entry.Key)
		case 2:
			err = s.buildInventoryCacheObject(ctx, writer, entry.Key)
		}
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
	}
	state.BuildToken = next
	if next == "" {
		state.BuildPhase++
	}
	return nil
}

func (s *Service) completeInventoryPlatforms(ctx context.Context, writer *inventory.Writer, token string) (bool, string, error) {
	requests, keys, next, err := writer.PendingPlatforms(ctx, token, s.inventoryConfig().BatchSize)
	if err != nil {
		return false, "", err
	}
	for n, request := range requests {
		id := inventory.ID("platform", request.Authority, request.Repository, request.Digest)
		ready, err := writer.HasImage(ctx, id)
		if err != nil {
			return false, "", err
		}
		if !ready {
			content, err := s.inventory.Content(ctx, request.Identity, request.Digest)
			if errors.Is(err, storage.ErrNotFound) {
				generation, err := s.generation(ctx, request.Authority, request.Repository, request.Digest)
				if err != nil {
					return false, "", err
				}
				content = inventory.Content{Identity: request.Identity, ObjectRef: request.ObjectRef, Generation: generation.Generation}
			} else if err != nil {
				return false, "", err
			}
			if err := s.buildInventoryPlatform(ctx, writer, content); err != nil {
				return false, "", err
			}
		}
		if err := writer.CompletePlatform(ctx, keys[n]); err != nil {
			return false, "", err
		}
	}
	return len(requests) == 0 && token == "" && next == "", next, nil
}

func (s *Service) publishInventory(ctx context.Context, state *inventoryCheckpoint, writer *inventory.Writer) error {
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	if writer.Snapshot.GCEpoch != gate.Epoch {
		return storage.ErrConditionFailed
	}
	if err := writer.Publish(ctx, state.Scan.Complete, state.Scan.Reasons); err != nil {
		return err
	}
	state.Build = nil
	state.Scan.Reasons = nil
	if s.metrics != nil {
		s.metrics.recordEvent("inventory_snapshot_published")
		s.metrics.inventoryPublished(writer.Snapshot)
	}
	return s.cleanupInventorySnapshots(ctx, writer.Snapshot.SnapshotID)
}
