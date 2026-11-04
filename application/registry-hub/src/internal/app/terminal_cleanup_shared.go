package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (s *Service) checkTerminalSharedDemand(ctx context.Context, c *protectionRecoveryCheckpoint) error {
	entry, ok, err := c.DemandCursor.Peek(ctx, s.store, "solidification/blobs/owners/"+c.Demand.IntentID+"/", 16)
	if err != nil {
		return err
	}
	if !ok {
		if c.DemandCursor.Done {
			c.Step = "demand_cancel"
		}
		return nil
	}
	demand, err := s.records.ReadRecoveryBlobDemand(ctx, entry.Key, c.Demand.IntentID)
	if errors.Is(err, storage.ErrNotFound) {
		c.DemandCursor.Advance()
		return nil
	}
	if err != nil {
		return err
	}
	active := demand.RootTaskID == ""
	if !active {
		root, err := s.tasks.Get(ctx, demand.RootTaskID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		active = err == nil && !root.TerminalCleanupComplete
	}
	if active {
		c.ActiveDemand, c.Step = true, "demand_delete"
	} else {
		c.DemandCursor.Advance()
	}
	return nil
}
