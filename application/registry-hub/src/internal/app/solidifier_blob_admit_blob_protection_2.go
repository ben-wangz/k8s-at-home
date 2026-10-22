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
