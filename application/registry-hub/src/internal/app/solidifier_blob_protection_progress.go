package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func (s *solidifier) admitAndSaveBlobProtection(ctx context.Context, task *tasks.Task, taskLease lease.Lease, intent tasks.BlobIntent) error {
	for attempt := 0; attempt < 4; attempt++ {
		base := *task
		protection, err := s.admitBlobProtection(ctx, task, intent)
		if errors.Is(err, ErrProtectionAnchored) {
			_, stateErr := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentAnchored, "", "")
			return stateErr
		}
		if err != nil {
			return err
		}
		task.ScopeID = protection.scopeID
		task.SessionIDs = []string{protection.sessionID}
		task.RequestedBytes, task.ReservedBytes = protection.reservedBytes, protection.reservedBytes
		task.GCProtected = true
		task.ProtectionSessionID, task.ProtectionOwnerID = protection.sessionID, protection.ownerID
		if reserved, exists := s.gate.ProtectedCacheReservation(task.ID); exists {
			task.ProtectedCacheBytes = reserved
		} else {
			task.ProtectedCacheBytes = 0
		}
		if err := s.tasks.SaveBlobProgressWithProtectionMerge(ctx, base, task, taskLease); err == nil {
			return nil
		} else if !errors.Is(err, tasks.ErrProtectionAdmissionConflict) {
			return err
		} else {
			admissionErr := err
			current, readErr := s.tasks.Get(ctx, task.ID)
			if readErr != nil {
				return errors.Join(admissionErr, readErr)
			}
			if retryErr := blobProtectionAdmissionRetryError(base, current, taskLease); retryErr != nil {
				return s.releaseRejectedBlobProtection(ctx, protection, intent.ID, current.ProtectionSessionID, retryErr)
			}
			if err := s.tasks.Leases().Validate(ctx, taskLease); err != nil {
				return s.releaseRejectedBlobProtection(ctx, protection, intent.ID, current.ProtectionSessionID, err)
			}
			if protection.sessionID != current.ProtectionSessionID {
				if _, err := s.gate.RemoveProtectionOwner(ctx, protection.sessionID, intent.ID); err != nil {
					return err
				}
			}
			*task = current
		}
	}
	return tasks.ErrProtectionAdmissionConflict
}

func blobProtectionAdmissionRetryError(base, current tasks.Task, taskLease lease.Lease) error {
	if current.CancelRequested || current.Status == tasks.Cancelled {
		return tasks.ErrCancelled
	}
	if current.ID != base.ID || current.Status != tasks.Running || base.AttemptID == "" || current.AttemptID != base.AttemptID ||
		current.Generation != base.Generation || current.RoundID != base.RoundID || current.FencingToken > taskLease.FencingToken {
		return lease.ErrLost
	}
	return nil
}

func (s *solidifier) releaseRejectedBlobProtection(ctx context.Context, protection blobProtection, ownerID, currentSession string, cause error) error {
	if protection.sessionID == currentSession {
		return cause
	}
	if _, err := s.gate.RemoveProtectionOwner(ctx, protection.sessionID, ownerID); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}
