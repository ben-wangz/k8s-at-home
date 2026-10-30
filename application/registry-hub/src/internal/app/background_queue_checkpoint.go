package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/workqueue"
	"time"
)

type queueSweepCheckpoint struct {
	Cursor     inventory.Cursor `json:"cursor"`
	Status     workqueue.Status `json:"status"`
	ScanFailed bool             `json:"scanFailed"`
	NextRound  time.Time        `json:"nextRound"`
	Fence      uint64           `json:"fence"`
}

func queueCheckpointKey(kind string) string {
	return workqueue.Prefix + "queue-checkpoints/" + kind + ".json"
}

func (s *Service) saveQueueSweep(ctx context.Context, holder lease.Lease, kind string, state queueSweepCheckpoint) error {
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
	summary := state.Status
	state.Fence = holder.FencingToken
	if state.Cursor.Done {
		state.Cursor, state.Status = inventory.Cursor{}, workqueue.NewStatus(time.Now().UTC())
		state.ScanFailed = false
		state.NextRound = time.Now().Add(time.Second)
	}
	if err := inventory.Update(ctx, s.store, queueCheckpointKey(kind), func(value *queueSweepCheckpoint) error {
		if value.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = state
		return nil
	}); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, workqueue.Prefix+"status/"+kind+".json", func(value *workqueue.Status) error {
		if value.LeaseFence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = summary
		return nil
	})
}

func (s *Service) runBackgroundQueue(ctx context.Context, kind string, step backgroundStep) {
	for ctx.Err() == nil {
		leaderCtx, holder, release, err := s.acquireBackgroundLease(ctx, kind, lease.NewManager(s.store), "background/queue/"+kind, s.instanceID+"/"+requestID(), time.Minute)
		if err == nil {
			s.runBackgroundQueueLeader(leaderCtx, holder, kind, step)
			_ = release()
		} else if !errors.Is(err, errBackgroundAdmissionYield) {
			s.backgroundEvent(kind, "lease_wait", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
