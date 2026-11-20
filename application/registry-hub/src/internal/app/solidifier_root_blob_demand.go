package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) ensureRootAttempt(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if err := s.tasks.CheckActive(ctx, task, taskLease); err != nil {
		return err
	}
	current, err := s.tasks.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.Kind != tasks.RootKind || current.Generation != task.Generation || current.RoundID != task.RoundID || current.AttemptID != task.AttemptID || current.Status != tasks.Running {
		return lease.ErrLost
	}
	return nil
}

func (s *solidifier) resolveRootBlobDemand(ctx context.Context, task *tasks.Task, taskLease lease.Lease, blob contentRef, scopeID, sessionID string, reservedBytes int64) (tasks.BlobIntent, error) {
	demandID := stagedDemandID(task.ID, scopeID)
	identity := tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest}
	for attempt := 0; attempt < 4; attempt++ {
		if err := s.ensureRootAttempt(ctx, task, taskLease); err != nil {
			return tasks.BlobIntent{}, err
		}
		generation, err := s.blobContentGeneration(ctx, task, blob.digest)
		if err != nil {
			return tasks.BlobIntent{}, temporary("generation", err)
		}
		intent, _, err := s.service.records.EnsureBlobIntent(ctx, identity, generation, demandID)
		if errors.Is(err, tasks.ErrDemandAdmissionWaiting) {
			return tasks.BlobIntent{}, s.waitForBlobIntentAdmission(ctx, identity)
		}
		if err != nil {
			if errors.Is(err, storage.ErrConditionFailed) {
				continue
			}
			return tasks.BlobIntent{}, temporary("task_state", err)
		}
		if intent.Generation != generation {
			continue
		}
		if err := s.service.records.LinkBlobDemandScope(ctx, intent.ID, demandID, task.ID, scopeID, sessionID, reservedBytes); errors.Is(err, tasks.ErrDemandAdmissionWaiting) {
			return tasks.BlobIntent{}, s.waitForBlobIntentAdmission(ctx, identity)
		} else if errors.Is(err, tasks.ErrDemandAdmissionClosed) || errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if err != nil {
			return tasks.BlobIntent{}, temporary("task_state", err)
		}
		current, err := s.service.records.GetBlobIntentByIdentity(ctx, identity)
		if err != nil {
			return tasks.BlobIntent{}, temporary("task_state", err)
		}
		currentGeneration, err := s.blobContentGeneration(ctx, task, blob.digest)
		if err != nil {
			return tasks.BlobIntent{}, temporary("generation", err)
		}
		if current.ID != intent.ID || currentGeneration != generation {
			_ = s.service.records.RemoveBlobDemand(ctx, tasks.BlobDemand{IntentID: intent.ID, DemandID: demandID, RootTaskID: task.ID})
			continue
		}
		if err := s.ensureRootAttempt(ctx, task, taskLease); err != nil {
			_ = s.service.records.RemoveBlobDemand(ctx, tasks.BlobDemand{IntentID: intent.ID, DemandID: demandID, RootTaskID: task.ID})
			return tasks.BlobIntent{}, err
		}
		return intent, nil
	}
	return tasks.BlobIntent{}, temporary("task_state", storage.ErrUnavailable)
}

func (s *solidifier) waitForBlobIntentAdmission(ctx context.Context, identity tasks.BlobIdentity) error {
	intent, err := s.service.records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	resource := "blob-intent/unknown"
	if err == nil {
		resource = "blob-intent/" + intent.ID
	}
	return tasks.WaitForResource(tasks.WaitingDeps, "blob_intent_admission", resource)
}
