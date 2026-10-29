package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"slices"
	"time"
)

type inventoryAvailability struct {
	nextCheckAt time.Time
	retryDelay  time.Duration
	pending     *inventory.Snapshot
	createdAt   time.Time
	previousID  string
}

func (s *Service) checkInventoryAvailability(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint, availability *inventoryAvailability) error {
	if time.Now().Before(availability.nextCheckAt) {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, s.inventoryAvailabilityTimeout())
	err := s.refreshInventoryPlaceholder(checkCtx, leader, state, availability)
	cancel()
	if err == nil {
		availability.retryDelay = 0
		availability.nextCheckAt = time.Now().Add(min(30*time.Second, s.inventoryConfig().SnapshotRetention/4))
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, lease.ErrLost) || errors.Is(err, storage.ErrLeaseLost) || errors.Is(err, inventory.ErrSuperseded) {
		return err
	}
	availability.retryDelay = min(max(time.Second, availability.retryDelay*2), time.Minute)
	availability.nextCheckAt = time.Now().Add(availability.retryDelay)
	if s.metrics != nil {
		s.metrics.recordEvent("inventory_placeholder_refresh_failed")
	}
	if s.logger != nil {
		s.logger.Warn("inventory placeholder refresh deferred", "error", err, "retry_after", availability.retryDelay)
	}
	return nil
}

func (s *Service) inventoryAvailabilityTimeout() time.Duration {
	return min(15*time.Second, max(time.Second, s.inventoryConfig().BatchTimeout/4))
}

func (s *Service) refreshInventoryPlaceholder(ctx context.Context, leader lease.Lease, state *inventoryCheckpoint, availability *inventoryAvailability) error {
	current, _, err := inventory.Read[inventory.Snapshot](ctx, s.store, inventory.Prefix+"current.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	// Cover the next check, a full work batch, maintenance, and publication I/O.
	lead := s.inventoryConfig().BatchTimeout + 5*time.Second + s.inventoryAvailabilityTimeout() + 30*time.Second
	if err == nil && (!initialInventoryPlaceholder(current) || time.Until(current.ExpiresAt) > lead) {
		availability.pending = nil
		return nil
	}
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	if availability.pending == nil || availability.previousID != current.SnapshotID || time.Since(availability.createdAt) > s.inventoryConfig().SnapshotRetention/2 {
		now := time.Now().UTC()
		writer := s.inventory.NewWriter(now, s.inventoryConfig().SnapshotRetention, leader.FencingToken)
		started := now
		if !current.BuildStartedAt.IsZero() {
			started = current.BuildStartedAt
		}
		if state.Build != nil && !started.Before(state.Build.BuildStartedAt) {
			started = state.Build.BuildStartedAt.Add(-time.Nanosecond)
		}
		// Identity uses creation time; ordering and observation retain their older boundary.
		writer.Snapshot.BuildStartedAt = started
		writer.Snapshot.ExpiresAt = started.Add(s.inventoryConfig().SnapshotRetention)
		availability.pending, availability.createdAt, availability.previousID = &writer.Snapshot, now, current.SnapshotID
	}
	writer := s.inventory.ResumeWriter(*availability.pending)
	for step := 0; step < 2; step++ {
		done, err := writer.Assemble(ctx, 1)
		if err != nil {
			return err
		}
		if done {
			break
		}
	}
	if err := s.tasks.Leases().Validate(ctx, leader); err != nil {
		return err
	}
	if err := writer.Publish(ctx, false, []string{"index_backfilling", "untagged_history_not_enumerable"}); err != nil {
		return err
	}
	availability.pending = nil
	if s.metrics != nil {
		s.metrics.recordEvent("inventory_placeholder_published")
	}
	return nil
}

func initialInventoryPlaceholder(snapshot inventory.Snapshot) bool {
	if !snapshot.Building || snapshot.Coverage != "partial" || snapshot.UniqueManifests != 0 || !slices.Contains(snapshot.Reasons, "index_backfilling") {
		return false
	}
	for _, pages := range snapshot.Pages {
		if pages != 0 {
			return false
		}
	}
	for _, count := range snapshot.Counts {
		if count != 0 {
			return false
		}
	}
	return true
}
