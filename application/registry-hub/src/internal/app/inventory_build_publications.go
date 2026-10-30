package app

import (
	"context"
	"time"
)

func (s *Service) captureInventoryBuildPublications(ctx context.Context, state *inventoryCheckpoint) error {
	if state.Publications == nil && (state.BuildPage != nil || state.ImageBuild != nil) {
		state.PublicationsCaptured = true
		return nil
	}
	if state.Publications == nil {
		state.Publications = &inventoryDeltaProgress{PublicationsOnly: true}
	}
	state.Publications.Fence = state.Fence
	reserve := min(5*time.Second, s.inventoryConfig().BatchTimeout/4)
	for step := 0; step < min(4, s.inventoryConfig().BatchSize); step++ {
		if deadline, ok := ctx.Deadline(); step > 0 && ok && time.Until(deadline) < reserve {
			break
		}
		started := time.Now()
		done, err := s.advanceInventoryDelta(ctx, state.Publications)
		if err != nil {
			return err
		}
		if done {
			state.Publications, state.PublicationsCaptured = nil, true
			return nil
		}
		reserve = max(reserve, 2*time.Since(started))
	}
	return nil
}
