package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func (s *solidifier) processBlob(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if task.BlobDigest == "" || task.BlobSize < 0 || task.MediaType == "" || task.CacheVersion == "" {
		return permanent("task_state", errors.New("blob task is missing its verified cache identity"))
	}
	intent, err := s.service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_read")
	}
	if intent.Identity.Authority != task.Authority || intent.Identity.Repository != task.Repository || intent.Identity.Digest != task.BlobDigest || intent.CacheVersion != task.CacheVersion || intent.ActualSize != task.BlobSize {
		return permanent("task_state", errors.New("blob task does not match its durable intent"))
	}
	if intent.CancelRequested || intent.State == tasks.IntentCancelled {
		return tasks.ErrCancelled
	}
	if intent.State == tasks.IntentAnchored {
		return nil
	}
	if err := s.service.checkContentGeneration(ctx, task.Authority, task.Repository, task.BlobDigest); err != nil {
		return permanent("generation", err)
	}
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
	task.RequestedBytes = protection.reservedBytes
	task.ReservedBytes = protection.reservedBytes
	task.GCProtected = true
	task.ProtectionSessionID = protection.sessionID
	task.ProtectionOwnerID = protection.ownerID
	if reserved, exists := s.gate.ProtectedCacheReservation(task.ID); exists {
		task.ProtectedCacheBytes = reserved
	} else {
		task.ProtectedCacheBytes = 0
	}
	if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	if err := s.service.records.SetBlobIntentProtectionSession(ctx, task.ID, protection.sessionID); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentRunning, "", ""); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	blob := contentRef{digest: task.BlobDigest, mediaType: task.MediaType, size: task.BlobSize}
	if err := s.ensureBlob(ctx, task, taskLease, blob); err != nil {
		var yielded *tasks.Yield
		if errors.As(err, &yielded) {
			state := tasks.IntentReady
			switch yielded.Status {
			case tasks.WaitingCache:
				state = tasks.IntentWaitingCache
			case tasks.WaitingQuota:
				state = tasks.IntentWaitingQuota
			}
			if _, stateErr := s.service.records.SetBlobIntentState(ctx, task.ID, state, "", ""); stateErr != nil {
				return temporary("task_state", stateErr)
			}
			return err
		}
		var failure *tasks.Failure
		state := tasks.IntentRetryWait
		class, message := "solidification", err.Error()
		if errors.As(err, &failure) {
			class = failure.Class
			message = failure.Err.Error()
			if failure.Permanent {
				state = tasks.IntentFailed
			}
		}
		_, _ = s.service.records.SetBlobIntentState(context.WithoutCancel(ctx), task.ID, state, class, message)
		return err
	}
	if err := s.checkpoint(ctx, task, taskLease, blob); err != nil {
		return err
	}
	if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentVerified, "", ""); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	if err := s.releaseProtectedCacheReservation(ctx, task); err != nil {
		return temporary("protection_state", err)
	}
	if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, task.BlobDigest); err != nil {
		return temporary("storage", err)
	}
	s.service.wakeBlobDemandRoots(ctx, task.ID)
	return tasks.WaitFor(tasks.VerifiedLocal, "awaiting_manifest_anchor")
}

func (s *solidifier) TaskYielded(ctx context.Context, task *tasks.Task) {
	if task.Kind != tasks.BlobKind {
		return
	}
	intent, err := s.service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		return
	}
	if intent.State == tasks.IntentAnchored {
		if task.Status == tasks.WaitingDeps || task.Status == tasks.VerifiedLocal {
			_, _ = s.tasks.Wake(ctx, task.ID)
		}
		return
	}
	if task.Status == tasks.VerifiedLocal {
		s.service.wakeBlobDemandRoots(ctx, task.ID)
	}
}

func (s *solidifier) releaseEmptyDirectBlobProtection(ctx context.Context, task *tasks.Task, taskLease lease.Lease, intent tasks.BlobIntent) error {
	sessionID := intent.ProtectionSessionID
	if sessionID == "" {
		return nil
	}
	scopeID, err := protectionScopeID(task.Authority, task.Repository, task.BlobDigest, task.Generation)
	if err != nil {
		return permanent("task_state", err)
	}
	expectedSessionID := hashID("blob-session", scopeID, fmt.Sprint(intent.Round))
	if sessionID != expectedSessionID {
		return nil
	}
	session, active, err := s.gate.protectionSession(ctx, sessionID)
	if err != nil {
		return temporary("protection_state", err)
	}
	if !active {
		return nil
	}
	if session.ScopeID != scopeID {
		return temporary("protection_state", errors.New("direct blob protection scope does not match its intent"))
	}
	if err := s.gate.ReleaseEmptyProtection(ctx, sessionID, intent.ID); err != nil {
		if errors.Is(err, ErrProtectionBusy) || errors.Is(err, ErrProtectionHasContent) {
			return nil
		}
		return temporary("protection_release", err)
	}
	if err := s.service.records.ClearBlobIntentProtectionSession(ctx, intent.ID, sessionID); err != nil {
		return temporary("task_state", err)
	}
	if task.ProtectionSessionID == sessionID {
		task.ScopeID = ""
		task.SessionIDs = nil
		task.RequestedBytes = 0
		task.ReservedBytes = 0
		task.GCProtected = false
		task.ProtectionSessionID = ""
		task.ProtectionOwnerID = ""
		if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
			return temporary("task_state", err)
		}
	}
	return nil
}

type blobProtection struct {
	scopeID       string
	sessionID     string
	ownerID       string
	reservedBytes int64
}
