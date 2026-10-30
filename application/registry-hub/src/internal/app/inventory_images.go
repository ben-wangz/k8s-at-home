package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sync"
)

func (s *Service) enrollInventoryImages(ctx context.Context, state *inventoryCheckpoint, writer *inventory.Writer, prefix string) error {
	images := []*inventoryImageProgress{}
	for range 4 {
		entry, ok, err := state.BuildPage.Peek(ctx, s.store, prefix, s.inventoryConfig().BatchSize)
		if err != nil || !ok {
			if err != nil {
				return err
			}
			break
		}
		state.ImageBuild = nil
		if err := s.startInventoryImage(ctx, state, writer, entry.Key); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if state.ImageBuild != nil {
			images = append(images, state.ImageBuild)
			state.BuildPage.Advance()
		}
	}
	state.ImageBuild, state.ParallelImages, state.ImagesEnrolled = nil, nil, len(images) > 0
	if len(images) > 0 {
		state.ImageBuild, state.ParallelImages = images[0], images[1:]
	}
	state.BuildToken = state.BuildPage.Token
	// No lane writes immutable image data until this enrolled batch is checkpointed.
	return s.boundInventoryImages(ctx, state)
}

func (s *Service) advanceInventoryImages(ctx context.Context, state *inventoryCheckpoint) error {
	images := append([]*inventoryImageProgress{state.ImageBuild}, state.ParallelImages...)
	if len(images) > 4 {
		return storage.ErrCorruptObject
	}
	done, failures := make([]bool, len(images)), make([]error, len(images))
	var workers sync.WaitGroup
	for index, image := range images {
		workers.Go(func() {
			writer := s.inventory.ResumeWriter(*state.Build)
			done[index], failures[index] = s.advanceInventoryImageUnit(ctx, writer, image)
		})
	}
	workers.Wait()
	if err := errors.Join(failures...); err != nil {
		return err
	}
	pending := images[:0]
	for index, image := range images {
		if !done[index] {
			pending = append(pending, image)
		}
	}
	state.ImageBuild, state.ParallelImages = nil, nil
	if len(pending) > 0 {
		state.ImageBuild, state.ParallelImages = pending[0], pending[1:]
	} else {
		state.ImagesEnrolled = false
	}
	return s.boundInventoryImages(ctx, state)
}

func (s *Service) boundInventoryImages(ctx context.Context, state *inventoryCheckpoint) error {
	if len(state.ParallelImages) == 0 {
		return nil
	}
	data, err := json.Marshal(append([]*inventoryImageProgress{state.ImageBuild}, state.ParallelImages...))
	if err != nil || len(data) <= 4<<20 {
		return err
	}
	for _, image := range state.ParallelImages {
		data, err := json.Marshal(image)
		if err != nil {
			return err
		}
		key := inventory.Prefix + "snapshots/" + state.Build.SnapshotID + "/deferred/" + inventory.ID("image-work", string(data)) + ".json"
		if err := s.inventory.Immutable(ctx, key, image); err != nil {
			return err
		}
		state.DeferredImages = append(state.DeferredImages, key)
	}
	state.ParallelImages = nil
	return nil
}

func (s *Service) resumeDeferredInventoryImage(ctx context.Context, state *inventoryCheckpoint) error {
	image, _, err := inventory.Read[inventoryImageProgress](ctx, s.store, state.DeferredImages[0])
	if err != nil {
		return err
	}
	state.ImageBuild, state.ImagesEnrolled = &image, true
	state.DeferredImages = state.DeferredImages[1:]
	return nil
}
