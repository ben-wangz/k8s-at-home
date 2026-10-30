package app

import (
	"context"
	"time"
)

func (s *Service) scanInventoryUnit(ctx context.Context, state *inventoryScan) error {
	reserve := min(5*time.Second, s.inventoryConfig().BatchTimeout/4)
	for step := 0; step < min(4, s.inventoryConfig().BatchSize); step++ {
		if deadline, ok := ctx.Deadline(); step > 0 && ok && time.Until(deadline) < reserve {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		prefix := ""
		if state.Root != nil {
			prefix = state.Root.Prefix
		}
		started := time.Now()
		if err := s.scanInventoryPage(ctx, state); err != nil {
			return err
		}
		if state.Phase == 0 && state.Complete {
			break
		}
		if state.Root != nil && state.Root.Prefix != prefix {
			// Commit a new capture identity before writing its immutable graph pages.
			break
		}
		reserve = max(reserve, 2*time.Since(started))
	}
	return nil
}
