package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) observeSolidificationBlob(ctx context.Context, task *tasks.Task, blob contentRef, sessionID, basis string) error {
	ownership, ok := ctx.Value(contentLeaseContextKey{}).(contentLeaseOwnership)
	if !ok || ownership.holder.Key != "leases/objects/"+objectLeaseKey(task.Authority, task.Repository, blob.digest) {
		return temporary("object_lease", errors.New("solidification observation requires the content lease"))
	}
	if err := ownership.manager.Validate(ctx, ownership.holder); err != nil {
		return temporary("object_lease", err)
	}
	generation, err := s.blobContentGeneration(ctx, task, blob.digest)
	if err != nil {
		return temporary("generation", err)
	}
	epoch := s.gate.State().Epoch
	if err := s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch); err != nil {
		return err
	}
	if err := ownership.manager.Validate(ctx, ownership.holder); err != nil {
		return temporary("object_lease", err)
	}
	if _, err := s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphBlob, blob.digest, blob.size, generation, epoch); err == nil {
		return s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	return s.recordBlobObservation(ctx, task, blob, sessionID, generation, epoch, basis)
}
