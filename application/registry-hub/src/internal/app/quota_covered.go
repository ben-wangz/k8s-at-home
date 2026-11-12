package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) coveredDistributionBlob(ctx context.Context, task tasks.Task) (bool, error) {
	if task.Kind != tasks.BlobKind || task.CancelRequested || task.ProtectionSessionID == "" || s.records == nil || s.cache == nil {
		return false, nil
	}
	intent, err := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: task.BlobDigest})
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if intent.ID != task.ID || intent.Generation != task.Generation || intent.CancelRequested || intent.ProtectionSessionID != task.ProtectionSessionID || intent.CacheVersion != task.CacheVersion || intent.ActualSize != task.BlobSize {
		return false, nil
	}
	if intent.State != tasks.IntentReady && intent.State != tasks.IntentRetryWait && intent.State != tasks.IntentWaitingQuota {
		return false, nil
	}
	session, active, err := s.gate.protectionSession(ctx, task.ProtectionSessionID)
	if err != nil || !active || session.ScopeID != task.ScopeID || session.State != "protected" && session.State != "anchoring" || !session.StopRequestedAt.IsZero() || session.UncertainRemoteResult {
		return false, err
	}
	if err := s.gate.verifyProtectionOwner(ctx, protectionOwnerKey(session.OwnerSetID, task.ID), session.OwnerSetID, task.ID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	cached, err := s.cache.Inspect(ctx, task.Authority, task.Repository, task.BlobDigest, task.MediaType)
	return err == nil && cached.Present && !cached.Unknown && cached.DataVersion == task.CacheVersion && cached.Size == task.BlobSize, err
}

func (s *Service) wakeCoveredBlob(ctx context.Context, task tasks.Task) error {
	waiting := task.Status == tasks.WaitingQuota && task.WaitReason == "distribution_quota"
	running := task.Status == tasks.Running && task.QuotaWaitReason == "distribution_quota"
	if !waiting && !running || task.CancelRequested {
		return nil
	}
	gate := s.gate.State()
	if gate.State != "open" || gate.RecoveryBarrier || s.recoveryStarting.Load() {
		return nil
	}
	covered, err := s.coveredDistributionBlob(ctx, task)
	if err != nil || !covered {
		return err
	}
	current := s.gate.State()
	if current.Epoch != gate.Epoch || current.RecoveryBarrier || current.State != "open" {
		return nil
	}
	_, err = s.tasks.WakeMatching(ctx, task)
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
