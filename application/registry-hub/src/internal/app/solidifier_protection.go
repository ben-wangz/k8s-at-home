package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func protectionLimits(cfg config.Config) (int, int64) {
	maxSessions := cfg.Solidification.MaxProtectionSessions
	if maxSessions < 1 {
		maxSessions = 1024
	}
	maxBytes := cfg.Solidification.MaxUnanchoredDistributionBytes
	if maxBytes < 1 {
		maxBytes = cfg.Limits.ClosureBytes
	}
	if maxBytes < 1 {
		maxBytes = 1 << 40
	}
	return maxSessions, maxBytes
}

func markQuotaWaitPosition(task *tasks.Task, reason string) time.Time {
	if task.QuotaWaitAt.IsZero() || task.QuotaWaitReason != reason {
		task.QuotaWaitAt = time.Now().UTC()
		task.QuotaWaitReason = reason
	}
	return task.QuotaWaitAt
}

func protectedCacheLimit(cfg config.Config) int64 {
	limit := cfg.Solidification.MaxProtectedCacheBytes
	if limit < 1 {
		limit = cfg.Limits.DownloadMaxBytes
	}
	if limit < 1 {
		limit = 200 << 30
	}
	return limit
}

func protectionScopeID(authority, repository, digest string, generation uint64) (string, error) {
	identity := struct {
		Authority  string `json:"authority"`
		Repository string `json:"repository"`
		Digest     string `json:"digest"`
		Generation uint64 `json:"generation"`
	}{authority, repository, digest, generation}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func hashID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

func taskProtectionSession(task *tasks.Task) string {
	if task.ProtectionSessionID != "" {
		return task.ProtectionSessionID
	}
	return task.ID
}

func isDigestReference(reference string) bool {
	return artifactDigest(reference) == nil
}

func (s *solidifier) beginProtectedOperation(ctx context.Context, task *tasks.Task) (func() error, error) {
	sessionID := taskProtectionSession(task)
	if task.ProtectionSessionID == "" {
		release, err := s.gate.Enter(ctx)
		if err != nil {
			return nil, temporary("gc_admission", err)
		}
		return func() error {
			release()
			return nil
		}, nil
	}
	finish, err := s.gate.BeginProtectionOperation(ctx, sessionID)
	if err != nil {
		return nil, temporary("protection_operation", err)
	}
	return func() error {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return finish(finishCtx)
	}, nil
}

func (s *solidifier) finishProtectedOperation(finish func() error, result *error) {
	if err := finish(); err != nil {
		*result = errors.Join(*result, temporary("protection_operation", err))
	}
}

func (s *solidifier) markRemoteWrite(ctx context.Context, task *tasks.Task, remoteWrite protectionRemoteWrite) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteStarted(ctx, taskProtectionSession(task), remoteWrite); err != nil {
		return temporary("protection_write_state", err)
	}
	return nil
}

func (s *solidifier) markRemoteContentObserved(ctx context.Context, task *tasks.Task) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	return s.gate.MarkProtectionContentObserved(ctx, task.ProtectionSessionID)
}

func (s *solidifier) resolveRemoteWrite(ctx context.Context, task *tasks.Task, remoteWrite protectionRemoteWrite) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteResolved(ctx, taskProtectionSession(task), remoteWrite); err != nil {
		return temporary("protection_write_state", err)
	}
	return nil
}

func (s *solidifier) TaskTerminal(ctx context.Context, task *tasks.Task) error {
	if hasLegacyRecoveryBlocker(*task) && !hasTaskProtectionReferences(*task) && !safeLegacyTerminalTask(*task) {
		return errors.New("legacy task recovery blocker cannot be cleared from terminal task state")
	}
	if task.Status != tasks.Failed && task.Status != tasks.Cancelled && task.Status != tasks.Solidified {
		return nil
	}
	if task.Kind == tasks.RootKind {
		if reason, err := s.revokeRootBlobDemands(ctx, task.ID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, reason, err)
		}
	}
	if task.Kind == tasks.BlobKind {
		switch task.Status {
		case tasks.Failed:
			_, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentFailed, task.ErrorClass, task.ErrorMessage)
			if err != nil {
				return err
			}
		case tasks.Solidified:
			if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentAnchored, "", ""); err != nil {
				return err
			}
		}
	}
	if task.Kind == tasks.BlobKind && task.Status == tasks.Cancelled {
		if err := s.service.records.RemoveBlobDemand(ctx, tasks.BlobDemand{IntentID: task.ID, DemandID: task.ID}); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, "demand_remove_unknown", err)
		}
	}
	if task.Kind == tasks.BlobKind {
		if err := s.releaseProtectedCacheReservation(ctx, task); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, nil, "cache_reservation_release_unknown", err)
		}
	}
	sessions, err := s.protectionSessionsOwnedBy(ctx, task.ID)
	if err != nil {
		return s.requireTaskProtectionRecovery(ctx, task, nil, "owner_lookup_unknown", err)
	}
	sessions = uniqueSessionIDs(append(sessions, task.SessionIDs...))
	for _, sessionID := range sessions {
		if sessionID == legacyGrandfatheredSessionID {
			session, active, err := s.gate.protectionSession(ctx, sessionID)
			if err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
			}
			if !active {
				continue
			}
			if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_identity_unknown", errors.New("legacy grandfathered session identity is invalid"))
			}
			remaining, err := s.gate.RemoveProtectionOwner(ctx, sessionID, task.ID)
			if err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "owner_removal_unknown", err)
			}
			if remaining == 0 {
				if err := s.releaseLegacyGrandfatheredSessionIfUnowned(ctx); err != nil {
					return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "legacy_release_unknown", err)
				}
			}
			continue
		}
		if task.Status == tasks.Solidified {
			if err := s.releaseAnchoredSession(ctx, sessionID); err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "anchor_release_unknown", err)
			}
			continue
		}
		remaining, err := s.gate.RemoveProtectionOwner(ctx, sessionID, task.ID)
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "owner_removal_unknown", err)
		}
		if remaining > 0 {
			continue
		}
		session, active, err := s.gate.protectionSession(ctx, sessionID)
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
		}
		if !active {
			continue
		}
		if session.State == "anchored" {
			if err := s.gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
				return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "anchor_release_unknown", err)
			}
			continue
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "remote_result_unknown", errors.New("terminal task still has an unresolved remote operation"))
		}
		stop, err := s.gate.RequestProtectionStop(ctx, sessionID, terminalProtectionReason(task.Status))
		if err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "stop_request_unknown", err)
		}
		if !stop {
			continue
		}
		if err := s.gate.MarkProtectionQuiescing(ctx, sessionID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "quiescence_unknown", err)
		}
		session, active, err = s.gate.protectionSession(ctx, sessionID)
		if err != nil || !active {
			if err == nil {
				continue
			}
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "session_read_unknown", err)
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "remote_result_unknown", errors.New("terminal task still has an unresolved remote operation"))
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, sessionID); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "quiescence_unknown", err)
		}
		if err := s.gate.ReleaseProtection(ctx, sessionID, terminalProtectionReason(task.Status)); err != nil {
			return s.requireTaskProtectionRecovery(ctx, task, []string{sessionID}, "release_unknown", err)
		}
	}
	if task.Kind == tasks.BlobKind && task.Status == tasks.Cancelled {
		if _, err := s.service.records.SetBlobIntentState(ctx, task.ID, tasks.IntentCancelled, "", ""); err != nil {
			return err
		}
	}
	s.wakeOldestQuotaTask(ctx)
	if task.Kind == tasks.BlobKind {
		s.service.wakeBlobDemandRoots(ctx, task.ID)
	}
	s.clearTaskProtection(task)
	return s.tasks.SaveTerminalProtection(ctx, task, false, "")
}

func (s *solidifier) revokeRootBlobDemands(ctx context.Context, rootTaskID string) (string, error) {
	token := ""
	for {
		demands, next, err := s.service.records.ListRootBlobDemandRecords(ctx, rootTaskID, token, 500)
		if err != nil {
			return "demand_list_unknown", err
		}
		for _, demand := range demands {
			if err := s.service.records.RemoveBlobDemand(ctx, demand); err != nil {
				return "demand_remove_unknown", err
			}
			if reason, err := s.cancelBlobIntentWithoutDemand(ctx, demand.IntentID); err != nil {
				if s.service.logger != nil {
					s.service.logger.Warn("root blob demand cancellation failed", "root_task_id", rootTaskID, "intent_id", demand.IntentID, "recovery_reason", reason, "error", err)
				}
				return reason, err
			}
			if err := s.service.records.DeleteRootBlobDemandRecord(ctx, demand); err != nil {
				return "demand_index_delete_unknown", err
			}
		}
		if next == "" {
			return "", nil
		}
		token = next
	}
}

func (s *solidifier) cancelBlobIntentWithoutDemand(ctx context.Context, intentID string) (string, error) {
	intent, err := s.service.records.GetBlobIntent(ctx, intentID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "demand_intent_read_unknown", err
	}
	active, err := s.hasActiveBlobDemand(ctx, intentID)
	if err != nil {
		return "demand_active_check_unknown", err
	}
	if active {
		s.service.wakeBlobDemandRoots(ctx, intentID)
		return "", nil
	}
	task, err := s.tasks.Get(ctx, intentID)
	if errors.Is(err, storage.ErrNotFound) {
		if intent.ProtectionSessionID != "" {
			remaining, removeErr := s.gate.RemoveProtectionOwner(ctx, intent.ProtectionSessionID, intentID)
			if removeErr != nil && !errors.Is(removeErr, storage.ErrNotFound) {
				return "demand_owner_remove_unknown", removeErr
			}
			if removeErr == nil && remaining == 0 {
				if err := s.releaseUnownedProtectionSession(ctx, intent.ProtectionSessionID); err != nil {
					return "demand_protection_release_unknown", err
				}
			}
		}
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentCancelled, "no_demand", "no active blob demands remain")
		}
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return "", nil
			}
			return "demand_intent_cancel_unknown", err
		}
		return "", nil
	}
	if err != nil {
		return "demand_task_read_unknown", err
	}
	switch task.Status {
	case tasks.RepairPending:
		return "", nil
	case tasks.Solidified:
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentAnchored, "", "")
		}
		if err != nil {
			return "demand_intent_anchor_unknown", err
		}
		return "", nil
	case tasks.Failed, tasks.Cancelled:
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentCancelled, "no_demand", "no active blob demands remain")
		}
		if err != nil {
			return "demand_intent_cancel_unknown", err
		}
		return "", nil
	default:
		_, err := s.tasks.Cancel(ctx, intentID)
		if err != nil {
			return "demand_task_cancel_unknown", err
		}
		return "", nil
	}
}

func (s *solidifier) releaseUnownedProtectionSession(ctx context.Context, sessionID string) error {
	session, active, err := s.gate.protectionSession(ctx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	stop, err := s.gate.RequestProtectionStop(ctx, sessionID, "cancelled")
	if err != nil || !stop {
		return err
	}
	session, active, err = s.gate.protectionSession(ctx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.State == "anchored" {
		return s.gate.ReleaseProtection(ctx, sessionID, "anchored")
	}
	if session.State != "quiesced" {
		if session.State != "quiescing" {
			if err := s.gate.MarkProtectionQuiescing(ctx, sessionID); err != nil {
				return err
			}
		}
		session, active, err = s.gate.protectionSession(ctx, sessionID)
		if err != nil || !active {
			return err
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return ErrProtectionBusy
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, sessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, sessionID, "cancelled")
}

func (s *solidifier) hasActiveBlobDemand(ctx context.Context, intentID string) (bool, error) {
	token := ""
	for {
		demands, next, err := s.service.records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, demand := range demands {
			if demand.RootTaskID == "" {
				return true, nil
			}
			root, err := s.tasks.Get(ctx, demand.RootTaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !root.CancelRequested && !isTerminalTask(root.Status) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func blobIntentNeedsSolidification(state tasks.BlobIntentState) bool {
	switch state {
	case tasks.IntentWaitingCache, tasks.IntentWaitingQuota, tasks.IntentReady, tasks.IntentRunning, tasks.IntentRetryWait, tasks.IntentVerified:
		return true
	default:
		return false
	}
}

func (s *solidifier) releaseLegacyGrandfatheredSessionIfUnowned(ctx context.Context) error {
	session, active, err := s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil || !active {
		return err
	}
	if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID {
		return errors.New("legacy grandfathered session identity is invalid")
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	stop, err := s.gate.RequestProtectionStop(ctx, legacyGrandfatheredSessionID, "cancelled")
	if err != nil || !stop {
		return err
	}
	session, active, err = s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	if session.State == "anchored" {
		return s.gate.ReleaseProtection(ctx, legacyGrandfatheredSessionID, "anchored")
	}
	if session.State != "quiesced" {
		if session.State != "quiescing" {
			if err := s.gate.MarkProtectionQuiescing(ctx, legacyGrandfatheredSessionID); err != nil {
				return err
			}
		}
		session, active, err = s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
		if err != nil || !active {
			return err
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return ErrProtectionBusy
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, legacyGrandfatheredSessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, legacyGrandfatheredSessionID, "cancelled")
}

func terminalProtectionReason(status tasks.State) string {
	if status == tasks.Failed {
		return "failed"
	}
	return "cancelled"
}

func (s *solidifier) protectionSessionsOwnedBy(ctx context.Context, ownerID string) ([]string, error) {
	var result []string
	token := ""
	for {
		sessions, next, err := s.gate.ListProtectionSessions(ctx, token, 500)
		if err != nil {
			return nil, err
		}
		for _, session := range sessions {
			owners, err := s.gate.ProtectionSessionOwners(ctx, session.ID)
			if err != nil {
				if errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return nil, err
			}
			for _, owner := range owners {
				if owner == ownerID {
					result = append(result, session.ID)
					break
				}
			}
		}
		if next == "" {
			return result, nil
		}
		token = next
	}
}

func (s *solidifier) releaseAnchoredSession(ctx context.Context, sessionID string) error {
	session, active, err := s.gate.protectionSession(ctx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	if session.State != "anchored" {
		if err := s.gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, sessionID, "anchored")
}

func (s *solidifier) wakeOldestQuotaTask(ctx context.Context) {
	if s.service == nil {
		return
	}
	if err := s.service.wakeAvailableQuotaWaiter(ctx, "distribution_quota", s.gate.State()); err != nil && s.service.logger != nil {
		s.service.logger.Warn("quota waiter wake failed", "error_class", "storage")
	}
}

func (s *solidifier) releaseProtectedCacheReservation(ctx context.Context, task *tasks.Task) error {
	hadReservation := s.gate.HasProtectedCacheReservation(task.ID)
	if err := s.gate.ReleaseProtectedCache(ctx, task.ID); err != nil {
		return err
	}
	task.ProtectedCacheBytes = 0
	if err := s.tasks.SetProtectedCacheBytes(ctx, task.ID, 0); err != nil {
		return err
	}
	if hadReservation {
		if s.service != nil {
			if err := s.service.wakeAvailableQuotaWaiter(ctx, "protected_cache_capacity", s.gate.State()); err != nil && s.service.logger != nil {
				s.service.logger.Warn("protected cache quota waiter wake failed", "error_class", "storage")
			}
		}
	}
	return nil
}

func (s *solidifier) clearTaskProtection(task *tasks.Task) {
	task.ScopeID = ""
	task.SessionIDs = nil
	task.RequestedBytes = 0
	task.ReservedBytes = 0
	task.GCProtected = false
	task.RecoveryReason = ""
	task.ProtectionSessionID = ""
	task.ProtectionOwnerID = ""
}

func (s *solidifier) requireTaskProtectionRecovery(ctx context.Context, task *tasks.Task, sessionIDs []string, reason string, cause error) error {
	ids := append([]string(nil), sessionIDs...)
	ids = append(ids, task.SessionIDs...)
	if task.ProtectionSessionID != "" {
		ids = append(ids, task.ProtectionSessionID)
	}
	unique := make(map[string]bool, len(ids))
	var markErr error
	activeIDs := make([]string, 0, len(ids))
	var reserved int64
	for _, id := range ids {
		if id == "" || unique[id] {
			continue
		}
		unique[id] = true
		if err := s.gate.MarkProtectionRecoveryRequired(ctx, id, reason); err != nil && !errors.Is(err, storage.ErrNotFound) {
			markErr = errors.Join(markErr, err)
		}
		session, active, err := s.gate.protectionSession(ctx, id)
		if err != nil {
			markErr = errors.Join(markErr, err)
			continue
		}
		if active {
			activeIDs = append(activeIDs, id)
			if task.ScopeID == "" {
				if id == legacyGrandfatheredSessionID && isLegacySolidificationTask(*task) {
					task.ScopeID, err = protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
					if err != nil {
						markErr = errors.Join(markErr, err)
					}
				} else {
					task.ScopeID = session.ScopeID
				}
			}
			sessionBytes, reserveErr := taskProtectionReservationBytes(task, id, session)
			if reserveErr != nil {
				markErr = errors.Join(markErr, reserveErr)
				sessionBytes = session.ReservedBytes
			}
			if sessionBytes > math.MaxInt64-reserved {
				markErr = errors.Join(markErr, errors.New("task protection reservation overflows"))
				reserved = math.MaxInt64
			} else {
				reserved += sessionBytes
			}
		}
	}
	barrierErr := s.gate.SetRecoveryBarrier(ctx, reason)
	task.SessionIDs = activeIDs
	task.RequestedBytes = reserved
	task.ReservedBytes = reserved
	task.GCProtected = true
	task.RecoveryReason = reason
	if saveErr := s.tasks.SaveTerminalProtection(ctx, task, true, reason); saveErr != nil {
		barrierErr = errors.Join(barrierErr, saveErr)
	}
	return errors.Join(cause, markErr, barrierErr)
}
