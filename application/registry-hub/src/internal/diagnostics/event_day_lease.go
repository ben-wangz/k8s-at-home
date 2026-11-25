package diagnostics

import (
	"context"
	"time"

	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

const eventDayLeaseTTL = 30 * time.Second

func acquireEventDayLease(ctx context.Context, store storage.Store, date string) (lease.Lease, error) {
	if !validEventDate(date) {
		return lease.Lease{}, storage.ErrCorruptObject
	}
	owner := ID("event-history-day", date, time.Now().UTC().Format(time.RFC3339Nano))
	return lease.NewManager(store).Acquire(ctx, "diagnostics/event-history-day/"+date, owner, eventDayLeaseTTL)
}

func releaseEventDayLease(ctx context.Context, store storage.Store, holder lease.Lease) error {
	err := lease.NewManager(store).Release(ctx, holder)
	if err == lease.ErrLost {
		return storage.ErrLeaseLost
	}
	return err
}
