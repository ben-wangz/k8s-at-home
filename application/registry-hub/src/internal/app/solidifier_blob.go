package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
)

func (s *solidifier) processBlob(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if task.BlobDigest == "" || task.BlobSize < 0 || task.MediaType == "" || task.CacheVersion == "" {
		return permanent("task_state", errors.New("blob task is missing its verified cache identity"))
	}
	intent, err := s.service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		return temporary("task_state", err)
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
		return temporary("task_state", err)
	}
	if err := s.service.records.SetBlobIntentProtectionSession(ctx, task.ID, protection.sessionID); err != nil {
		return temporary("task_state", err)
	}
	if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentRunning, "", ""); err != nil {
		return temporary("task_state", err)
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
		return temporary("task_state", err)
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

func (s *solidifier) admitBlobProtection(ctx context.Context, task *tasks.Task, intent tasks.BlobIntent) (blobProtection, error) {
	demands, err := s.listBlobDemands(ctx, intent.ID)
	if err != nil {
		return blobProtection{}, temporary("task_state", err)
	}
	directDemand := false
	var candidates []tasks.BlobDemand
	for _, demand := range demands {
		if demand.DemandID == intent.ID && demand.ScopeID == "" {
			directDemand = true
		}
		if demand.RootTaskID != "" && demand.ScopeID != "" && demand.SessionID != "" {
			candidates = append(candidates, demand)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].SessionID == candidates[j].SessionID {
			return candidates[i].RootTaskID < candidates[j].RootTaskID
		}
		return candidates[i].SessionID < candidates[j].SessionID
	})
	for _, candidate := range candidates {
		root, err := s.tasks.Get(ctx, candidate.RootTaskID)
		if err != nil || root.CancelRequested || root.Status == tasks.Cancelled || root.Status == tasks.Failed || root.Status == tasks.Solidified {
			continue
		}
		session, active, err := s.gate.protectionSession(ctx, candidate.SessionID)
		if err != nil {
			return blobProtection{}, temporary("protection_state", err)
		}
		if !active || session.ScopeID != candidate.ScopeID || session.State != "protected" && session.State != "anchoring" {
			continue
		}
		if err := s.gate.AddProtectionOwner(ctx, candidate.SessionID, intent.ID); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return blobProtection{}, temporary("protection_owner", err)
		}
		return blobProtection{scopeID: candidate.ScopeID, sessionID: candidate.SessionID, ownerID: candidate.RootTaskID, reservedBytes: session.ReservedBytes}, nil
	}
	if !directDemand {
		if len(candidates) > 0 {
			return blobProtection{}, tasks.WaitFor(tasks.WaitingDeps, "manifest_scope_admission")
		}
		return blobProtection{}, tasks.WaitFor(tasks.WaitingDeps, "awaiting_manifest_demand")
	}
	scopeID, err := protectionScopeID(task.Authority, task.Repository, task.BlobDigest, task.Generation)
	if err != nil {
		return blobProtection{}, permanent("task_state", err)
	}
	maxSessions, maxBytes := protectionLimits(s.cfg)
	if task.BlobSize > maxBytes {
		return blobProtection{}, permanent("capacity_limit", errors.New("blob scope exceeds the unanchored Distribution byte limit"))
	}
	distributionTurn, err := s.tasks.IsQuotaTurnAt(ctx, task.ID, "distribution_quota", markQuotaWaitPosition(task, "distribution_quota"))
	if err != nil {
		return blobProtection{}, temporary("task_state", err)
	}
	if !distributionTurn {
		return blobProtection{}, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
	}
	sessionID := hashID("blob-session", scopeID, fmt.Sprint(intent.Round))
	if err := s.gate.AdmitProtection(ctx, sessionID, scopeID, intent.ID, task.BlobSize, maxSessions, maxBytes); err != nil {
		if errors.Is(err, ErrProtectionAnchored) {
			return blobProtection{}, ErrProtectionAnchored
		}
		if errors.Is(err, ErrProtectionCapacity) {
			return blobProtection{}, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
		}
		if errors.Is(err, ErrWritesClosed) || errors.Is(err, ErrRecoveryBarrier) {
			return blobProtection{}, tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
		}
		return blobProtection{}, temporary("protection_admission", err)
	}
	return blobProtection{scopeID: scopeID, sessionID: sessionID, ownerID: intent.ID, reservedBytes: task.BlobSize}, nil
}

func (s *solidifier) reserveBlobCacheInput(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	limit := protectedCacheLimit(s.cfg)
	if task.BlobSize > limit {
		return permanent("capacity_limit", errors.New("blob input exceeds the protected cache byte limit"))
	}
	if reserved, exists := s.gate.ProtectedCacheReservation(task.ID); exists {
		if reserved != task.BlobSize {
			return temporary("protected_cache_reservation", errors.New("protected cache reservation does not match the blob size"))
		}
		task.ProtectedCacheBytes = reserved
		return nil
	}
	turn, err := s.tasks.IsQuotaTurnAt(ctx, task.ID, "protected_cache_capacity", markQuotaWaitPosition(task, "protected_cache_capacity"))
	if err != nil {
		return temporary("task_state", err)
	}
	if !turn {
		return tasks.WaitFor(tasks.WaitingQuota, "protected_cache_capacity")
	}
	if err := s.gate.ReserveProtectedCache(ctx, task.ID, task.BlobSize, limit); err != nil {
		if errors.Is(err, ErrProtectedCacheCapacity) || errors.Is(err, ErrProtectionCapacity) {
			return tasks.WaitFor(tasks.WaitingQuota, "protected_cache_capacity")
		}
		if errors.Is(err, ErrWritesClosed) || errors.Is(err, ErrRecoveryBarrier) {
			return tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
		}
		return temporary("protected_cache_reservation", err)
	}
	task.ProtectedCacheBytes = task.BlobSize
	if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func (s *solidifier) listBlobDemands(ctx context.Context, intentID string) ([]tasks.BlobDemand, error) {
	var result []tasks.BlobDemand
	token := ""
	for {
		items, next, err := s.service.records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return result, nil
			}
			return nil, err
		}
		result = append(result, items...)
		if next == "" {
			return result, nil
		}
		token = next
	}
}

func (s *solidifier) wakeCoordinationWaiters(ctx context.Context) {
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 500)
		if err != nil {
			return
		}
		for _, task := range items {
			if task.Status == tasks.WaitingDeps && (task.WaitReason == "gc_admission" || task.WaitReason == "protection_transfer" || task.WaitReason == "manifest_scope_admission") && !task.CancelRequested {
				_, _ = s.tasks.Wake(ctx, task.ID)
			}
		}
		if next == "" {
			return
		}
		token = next
	}
}
