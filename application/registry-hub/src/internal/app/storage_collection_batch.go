package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const storageCollectionQuantumTarget = 8 * time.Second

func (s *Service) storageCollectionQuantum(ctx context.Context, owner lease.Lease, state *storageCollection, registry storage.Store, registryErr error) (maintenanceProgress, error) {
	started, reserve := time.Now(), time.Second
	progress := maintenanceProgress{}
	ctx = context.WithValue(ctx, storageReadCacheKey{}, newStorageReadCache())
	for step := 0; step < 8; step++ {
		if step > 0 && (time.Since(started)+reserve > storageCollectionQuantumTarget || !storageQuantumTime(ctx, reserve)) {
			before, err := json.Marshal(state)
			if err != nil {
				return progress, err
			}
			state.BudgetYields++
			state.LastYieldReason = "execution_budget"
			if err := s.checkpointStorageCollectionTransition(ctx, owner, state, before); err != nil {
				return progress, err
			}
			break
		}
		unit := time.Now()
		phase := storageCollectionPhase(*state)
		if err := s.tasks.Leases().Validate(ctx, owner); err != nil {
			return progress, err
		}
		if state.Domain == 1 && registry == nil && registryErr == nil {
			break
		}
		before, err := json.Marshal(state)
		if err != nil {
			return progress, err
		}
		err = s.advanceStorageCollection(ctx, state, registry, registryErr)
		if err != nil {
			pageSize := state.ListPageSize
			*state = storageCollection{}
			if restoreErr := json.Unmarshal(before, state); restoreErr != nil {
				return progress, restoreErr
			}
			state.ListPageSize, state.ListSuccesses = pageSize, 0
		}
		if err == nil {
			state.ProgressAt = time.Now().UTC()
		}
		if checkpointErr := s.checkpointStorageCollectionTransition(ctx, owner, state, before); checkpointErr != nil {
			return progress, errors.Join(err, checkpointErr)
		}
		if s.metrics != nil && s.metrics.storageCollection != nil {
			s.metrics.storageCollection.WithLabelValues("transition", phase).Observe(time.Since(unit).Seconds())
		}
		if err != nil {
			return progress, err
		}
		progress.EntityProgress, progress.RoundComplete, progress.NextScanAt = true, !state.NextScanAt.IsZero(), state.NextScanAt
		reserve = max(time.Second, 2*time.Since(unit))
		if progress.RoundComplete {
			break
		}
	}
	return progress, nil
}

func storageQuantumTime(ctx context.Context, reserve time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ctx.Err() == nil && (!ok || time.Until(deadline) > reserve)
}

func (s *Service) checkpointStorageCollection(ctx context.Context, owner lease.Lease, state *storageCollection) error {
	return s.checkpointStorageCollectionTransition(ctx, owner, state, nil)
}

func (s *Service) checkpointStorageCollectionTransition(ctx context.Context, owner lease.Lease, state *storageCollection, before []byte) error {
	started := time.Now()
	defer func() {
		if s.metrics != nil && s.metrics.storageCollection != nil {
			s.metrics.storageCollection.WithLabelValues("checkpoint", "all").Observe(time.Since(started).Seconds())
		}
	}()
	parent, ok := ctx.Value(backgroundParentKey{}).(context.Context)
	if !ok {
		parent = ctx
	}
	bounded, cancel := s.backgroundCheckpoint(parent, "storage_inventory")
	defer cancel()
	bounded = inventory.WithWriteGuard(bounded, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, owner) })
	state.CheckpointAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(data) > 4<<20 && (state.SourceListed || !state.Pending) {
		return storage.ErrCorruptObject
	}
	return inventory.Update(bounded, s.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error {
		current, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if sameStorageCollectionTransition(*value, *state) {
			return nil
		}
		if before != nil && !bytes.Equal(current, before) {
			return storage.ErrConditionFailed
		}
		*value = *state
		return nil
	})
}

func sameStorageCollectionTransition(left, right storageCollection) bool {
	left.CheckpointAt, right.CheckpointAt = time.Time{}, time.Time{}
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}
