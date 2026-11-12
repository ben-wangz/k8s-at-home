package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

type contentLeaseContextKey struct{}

type contentLeaseOwnership struct {
	manager *lease.Manager
	holder  lease.Lease
}

func (s *solidifier) coordinateBlobVerification(ctx context.Context, task *tasks.Task, blob contentRef, sessionID, path string, generation, epoch uint64) (result error) {
	started, outcome, record := time.Now(), "error", true
	defer func() {
		var yielded *tasks.Yield
		if result != nil && !errors.As(result, &yielded) {
			outcome = "error"
		}
		if record && s.service != nil {
			s.service.metrics.recordDistributionVerification(outcome, started)
		}
	}()
	if err := s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch); err != nil {
		return err
	}
	_, err := s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphBlob, blob.digest, blob.size, generation, epoch)
	if err == nil {
		outcome = "reuse"
		return s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	key := "leases/objects/" + objectLeaseKey(task.Authority, task.Repository, blob.digest)
	ownership, owned := ctx.Value(contentLeaseContextKey{}).(contentLeaseOwnership)
	if owned && ownership.holder.Key == key {
		if err := ownership.manager.Validate(ctx, ownership.holder); err != nil {
			return temporary("object_lease", err)
		}
		record = false
		return s.verifyBlobUnderLease(ctx, task, blob, sessionID, path, generation, epoch, ownership)
	}
	outcome = "wait"
	return s.deferBlobVerification(ctx, task, blob, sessionID, generation, epoch)
}

func (s *solidifier) verifyBlobUnderLease(ctx context.Context, task *tasks.Task, blob contentRef, sessionID, path string, generation, epoch uint64, ownership contentLeaseOwnership) (result error) {
	started, outcome := time.Now(), "error"
	defer func() {
		if result != nil {
			outcome = "error"
		}
		if s.service != nil {
			s.service.metrics.recordDistributionVerification(outcome, started)
		}
	}()
	if err := s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch); err != nil {
		return err
	}
	_, err := s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphBlob, blob.digest, blob.size, generation, epoch)
	if err == nil {
		outcome = "reuse"
		return s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch)
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	outcome = "readback"
	if err := s.verifyLocalObject(ctx, path, blob.digest, blob.size); err != nil {
		return err
	}
	if err := ownership.manager.Validate(ctx, ownership.holder); err != nil {
		return temporary("object_lease", err)
	}
	guarded := inventory.WithWriteGuard(ctx, func(check context.Context) error {
		return ownership.manager.Validate(check, ownership.holder)
	})
	return s.recordVerifiedBlobObservation(guarded, task, blob, sessionID, generation, epoch)
}
