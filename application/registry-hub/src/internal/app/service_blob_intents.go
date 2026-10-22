package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) ensureBlobIntent(ctx context.Context, routed route.Route) (tasks.BlobIntent, error) {
	return s.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
		Authority: routed.Upstream, Repository: routed.RemoteRepo, Digest: routed.Reference,
	})
}

func (s *Service) ensureBlobIntentForIdentity(ctx context.Context, identity tasks.BlobIdentity) (tasks.BlobIntent, error) {
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		return tasks.BlobIntent{}, err
	}
	intent, _, err := s.records.EnsureBlobIntent(ctx, identity, generation.Generation, "")
	if err != nil {
		return tasks.BlobIntent{}, err
	}
	if err := s.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		return tasks.BlobIntent{}, err
	}
	return intent, nil
}

func (s *Service) activateBlobIntent(ctx context.Context, intent tasks.BlobIntent, cached artifact.CachedObject) error {
	ready, err := s.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		return err
	}
	if ready.ID != intent.ID {
		if err := s.records.LinkBlobDemand(ctx, ready.ID, ready.ID); err != nil {
			return err
		}
	}
	if ready.State == tasks.IntentAnchored || ready.State == tasks.IntentVerified {
		return nil
	}
	if ready.State != tasks.IntentReady && ready.State != tasks.IntentRetryWait {
		return nil
	}
	blobTask, _, enqueueErr := s.enqueueBlobIntent(ctx, ready)
	if enqueueErr != nil && s.logger != nil {
		s.logger.Warn("blob intent is ready but task admission is deferred", "intent_id", ready.ID, "error_class", "queue")
	} else {
		ready.ID = blobTask.ID
		if blobTask.Status == tasks.VerifiedLocal || blobTask.Status == tasks.WaitingDeps && blobTask.WaitReason == "manifest_scope_admission" {
			_, _ = s.tasks.Wake(ctx, blobTask.ID)
		}
	}
	s.wakeBlobDemandRoots(ctx, ready.ID)
	return nil
}

func (s *Service) enqueueBlobIntent(ctx context.Context, intent tasks.BlobIntent) (tasks.Task, bool, error) {
	current, err := s.records.GetBlobIntentByIdentity(ctx, intent.Identity)
	if err != nil {
		return tasks.Task{}, false, err
	}
	if current.State != tasks.IntentReady && current.State != tasks.IntentRetryWait {
		return tasks.Task{}, false, storage.ErrConditionFailed
	}
	intent = current
	var session protectionSessionRecord
	protected := false
	if intent.ProtectionSessionID != "" {
		if s.gate.State().RecoveryBarrier {
			return tasks.Task{}, false, ErrRecoveryBarrier
		}
		var active bool
		var err error
		session, active, err = s.gate.protectionSession(ctx, intent.ProtectionSessionID)
		if err != nil {
			return tasks.Task{}, false, err
		}
		if !active {
			persisted, readErr := s.gate.readProtectionSession(ctx, intent.ProtectionSessionID)
			if errors.Is(readErr, storage.ErrNotFound) {
				barrierErr := s.gate.SetRecoveryBarrier(ctx, "blob_owner_unknown")
				return tasks.Task{}, false, errors.Join(ErrRecoveryBarrier, barrierErr)
			}
			if readErr != nil {
				return tasks.Task{}, false, readErr
			}
			switch persisted.State {
			case "transferred":
				return tasks.Task{}, false, ErrRecoveryBarrier
			case "released":
				if persisted.TerminalReason == "anchored" {
					return tasks.Task{}, false, ErrProtectionAnchored
				}
				return tasks.Task{}, false, storage.ErrConditionFailed
			default:
				barrierErr := s.gate.SetRecoveryBarrier(ctx, "blob_owner_unknown")
				return tasks.Task{}, false, errors.Join(ErrRecoveryBarrier, barrierErr)
			}
		}
		if session.State != "protected" && session.State != "anchoring" {
			return tasks.Task{}, false, ErrProtectionAnchored
		}
		if err := s.gate.AddProtectionOwner(ctx, session.ID, intent.ID); err != nil {
			return tasks.Task{}, false, err
		}
		protected = true
	}
	var task tasks.Task
	var created bool
	if protected {
		task, created, err = s.tasks.EnqueueBlobProtected(ctx, intent, session.ScopeID, session.ReservedBytes)
	} else {
		task, created, err = s.tasks.EnqueueBlob(ctx, intent)
	}
	if err != nil {
		return tasks.Task{}, false, err
	}
	if protected {
		if err := s.tasks.ReconcileProtectionState(ctx, task.ID, session.ScopeID, []string{session.ID}, session.ReservedBytes, session.ReservedBytes); err != nil {
			return task, created, err
		}
	}
	return task, created, nil
}
