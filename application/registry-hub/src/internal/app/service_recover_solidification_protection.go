package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
	"time"
)

const legacyGrandfatheredSessionID = "f2b4944d5d74f22a6a4e357fa693e96d"

const legacyGrandfatheredScopeID = "legacy-grandfathered"

func (s *Service) recoverSolidificationProtection(ctx context.Context) error {
	s.protectionRecoveryMu.Lock()
	defer s.protectionRecoveryMu.Unlock()
	if _, err := s.gate.BeginStartupRecovery(ctx); err != nil {
		return err
	}
	s.cacheProtectionReady.Store(false)
	if err := s.gate.WaitRecoveryQuiesced(ctx, 5*time.Minute); err != nil {
		return err
	}
	if err := s.gate.ValidateProtectionState(ctx); err != nil {
		return err
	}
	if err := s.recoverProtectionRemoteOperations(ctx); err != nil {
		return err
	}
	if err := s.rebuildCacheProtectionIndex(ctx); err != nil {
		return err
	}
	maxSessions, _ := protectionLimits(s.cfg)
	processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
	if err := s.prepareLegacyGrandfatheredProtection(ctx, maxSessions); err != nil {
		return err
	}
	activeCacheReservations := make(map[string]int64)
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 1000)
		if err != nil {
			return err
		}
		for _, task := range items {
			if task.Kind == tasks.BlobKind {
				reserved, exists := s.gate.ProtectedCacheReservation(task.ID)
				active := (task.Status == tasks.Pending || task.Status == tasks.Running || task.Status == tasks.RetryWait) && task.CacheVersion != "" && task.BlobSize >= 0
				if active && exists && reserved == task.BlobSize {
					activeCacheReservations[task.ID] = reserved
					if task.ProtectedCacheBytes != reserved {
						if err := s.tasks.SetProtectedCacheBytes(ctx, task.ID, reserved); err != nil {
							return err
						}
					}
				} else if task.ProtectedCacheBytes != 0 {
					if err := s.tasks.SetProtectedCacheBytes(ctx, task.ID, 0); err != nil {
						return err
					}
				}
			}
			if isTerminalTask(task.Status) {
				if isLegacySolidificationTask(task) && !hasTaskProtectionReferences(task) && !safeLegacyTerminalTask(task) {
					if err := s.reconcileTaskProtection(ctx, processor, task); err != nil {
						reason := recoveryBlockReason(task, err)
						if reasonErr := s.tasks.SetRecoveryReason(ctx, task.ID, reason); reasonErr != nil {
							return errors.Join(err, reasonErr)
						}
						return err
					}
					task, err = s.tasks.Get(ctx, task.ID)
					if err != nil {
						return err
					}
				}
				if isLegacySolidificationTask(task) && !hasTaskProtectionReferences(task) && !safeLegacyTerminalTask(task) {
					reason := task.RecoveryReason
					if reason == "" {
						reason = recoveryBlockReason(task, errors.New("legacy terminal task has no recoverable protection session"))
					}
					if err := s.tasks.SetRecoveryReason(ctx, task.ID, reason); err != nil {
						return err
					}
					return errors.New("legacy terminal task requires protection recovery")
				}
				if hasLegacyRecoveryBlocker(task) && !hasTaskProtectionReferences(task) && !safeLegacyTerminalTask(task) {
					return errors.New("legacy task recovery blocker cannot be cleared from terminal task state")
				}
				if !task.TerminalCleanupComplete || task.GCProtected || len(task.SessionIDs) > 0 || task.ProtectionSessionID != "" || task.RecoveryReason != "" || isLegacySolidificationTask(task) {
					if err := processor.TaskTerminal(ctx, &task); err != nil {
						return err
					}
				}
				continue
			}
			if err := s.reconcileTaskProtection(ctx, processor, task); err != nil {
				reason := recoveryBlockReason(task, err)
				if reasonErr := s.tasks.SetRecoveryReason(ctx, task.ID, reason); reasonErr != nil {
					return errors.Join(err, reasonErr)
				}
				return err
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if err := processor.releaseLegacyGrandfatheredSessionIfUnowned(ctx); err != nil {
		return err
	}
	if err := s.gate.ReconcileProtectedCacheReservations(ctx, activeCacheReservations); err != nil {
		return err
	}
	if err := s.gate.CompleteStartupRecovery(ctx); err != nil {
		return err
	}
	s.cacheProtectionReady.Store(true)
	return nil
}

func hasLegacyRecoveryBlocker(task tasks.Task) bool {
	if !isLegacySolidificationTask(task) {
		return false
	}
	switch task.RecoveryReason {
	case "legacy_uncheckpointed_write", "legacy_scope_over_limit", "legacy_scope_unbounded", "legacy_scope_recovery_failed":
		return true
	default:
		return false
	}
}

func isLegacySolidificationTask(task tasks.Task) bool {
	return task.ProtocolVersion < 2 && task.Kind == tasks.RootKind && task.Phase == "solidification"
}

func hasTaskProtectionReferences(task tasks.Task) bool {
	return len(task.SessionIDs) > 0 || task.ProtectionSessionID != ""
}

func (s *Service) prepareLegacyGrandfatheredProtection(ctx context.Context, maxSessions int) error {
	session, active, err := s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil {
		return err
	}
	owners := make(map[string]struct{})
	if active {
		if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID {
			return errors.New("legacy grandfathered session has an incompatible identity")
		}
		ownerIDs, err := s.gate.ProtectionSessionOwners(ctx, legacyGrandfatheredSessionID)
		if err != nil {
			return err
		}
		for _, ownerID := range ownerIDs {
			owners[ownerID] = struct{}{}
		}
	}

	hardLegacyLimit := s.cfg.Limits.ClosureBytes
	if hardLegacyLimit < 1 {
		hardLegacyLimit = 1 << 40
	}
	var requiredBytes int64
	requiredOwners := make(map[string]struct{})
	seenOwners := make(map[string]struct{}, len(owners))
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 1000)
		if err != nil {
			return err
		}
		for _, task := range items {
			_, hasOwner := owners[task.ID]
			if !isLegacySolidificationTask(task) {
				if hasOwner {
					return errors.New("legacy grandfathered session owner is not a legacy root task")
				}
				continue
			}
			if hasOwner {
				seenOwners[task.ID] = struct{}{}
			}
			if !legacyTaskNeedsProtection(task, hasOwner) {
				continue
			}
			if hasOwner && isTerminalTask(task.Status) && safeLegacyTerminalTask(task) {
				continue
			}
			if len(task.Checkpoints) == 0 {
				return s.failLegacyProtectionRecovery(ctx, task, errors.New("legacy root task may contain an uncheckpointed Distribution write"))
			}
			reservedBytes := task.LegacyProtectionBytes
			if reservedBytes == 0 {
				reservedBytes, err = taskGraphBytes(task)
				if err != nil {
					return s.failLegacyProtectionRecovery(ctx, task, err)
				}
				if err := s.tasks.SetLegacyProtectionBytes(ctx, task.ID, reservedBytes); err != nil {
					return err
				}
			}
			if reservedBytes < 1 || reservedBytes > hardLegacyLimit || reservedBytes > math.MaxInt64-requiredBytes {
				return s.failLegacyProtectionRecovery(ctx, task, ErrProtectionCapacity)
			}
			requiredBytes += reservedBytes
			requiredOwners[task.ID] = struct{}{}
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(seenOwners) != len(owners) {
		return errors.New("legacy grandfathered session has an owner without a matching task")
	}
	if active && session.ReservedBytes < requiredBytes {
		return errors.New("legacy grandfathered reservation is smaller than its active task scopes")
	}
	if active && session.State == "anchored" {
		if requiredBytes > 0 {
			return errors.New("active legacy tasks reference an already anchored grandfathered session")
		}
		return nil
	}
	if requiredBytes == 0 && !active {
		return nil
	}
	reservedBytes := requiredBytes
	if active {
		reservedBytes = session.ReservedBytes
	}
	if err := s.gate.AdmitProtectionDuringRecovery(ctx, legacyGrandfatheredSessionID, legacyGrandfatheredScopeID, "", reservedBytes, maxSessions, hardLegacyLimit); err != nil {
		return err
	}

	token = ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 1000)
		if err != nil {
			return err
		}
		for _, task := range items {
			if _, required := requiredOwners[task.ID]; !required {
				continue
			}
			if err := s.gate.AddProtectionOwnerDuringRecovery(ctx, legacyGrandfatheredSessionID, task.ID); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func legacyTaskNeedsProtection(task tasks.Task, hasOwner bool) bool {
	if !isLegacySolidificationTask(task) {
		return false
	}
	if isTerminalTask(task.Status) && safeLegacyTerminalTask(task) {
		return hasOwner
	}
	return true
}

func (s *Service) failLegacyProtectionRecovery(ctx context.Context, task tasks.Task, cause error) error {
	reason := recoveryBlockReason(task, cause)
	if err := s.tasks.SetRecoveryReason(ctx, task.ID, reason); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func safeLegacyTerminalTask(task tasks.Task) bool {
	if task.Status == tasks.Solidified && (task.RootPublished || isDigestReference(task.Reference)) {
		return true
	}
	return (task.Status == tasks.Cancelled || task.Status == tasks.Failed) && !task.RootPublishing && !task.RootPublished && len(task.Graph) == 0 && len(task.Checkpoints) == 0
}

func isTerminalTask(status tasks.State) bool {
	return status == tasks.Failed || status == tasks.Cancelled || status == tasks.Solidified
}

func (s *Service) reconcileTaskProtection(ctx context.Context, processor *solidifier, task tasks.Task) error {
	owned, err := processor.protectionSessionsOwnedBy(ctx, task.ID)
	if err != nil {
		return err
	}
	sessionIDs := append(append([]string(nil), task.SessionIDs...), owned...)
	if task.Kind == tasks.BlobKind {
		intent, intentErr := s.records.GetBlobIntent(ctx, task.ID)
		if intentErr != nil && !errors.Is(intentErr, storage.ErrNotFound) {
			return intentErr
		}
		if intentErr == nil && intent.ProtectionSessionID != "" {
			sessionIDs = append(sessionIDs, intent.ProtectionSessionID)
		}
	}
	sessionIDs = uniqueSessionIDs(sessionIDs)
	legacyTask := isLegacySolidificationTask(task)
	if legacyTask {
		sessionIDs = uniqueSessionIDs(append(sessionIDs, legacyGrandfatheredSessionID))
	}

	if len(sessionIDs) > 0 {
		activeIDs := make([]string, 0, len(sessionIDs))
		var reservedBytes int64
		scopeID := task.ScopeID
		for _, sessionID := range sessionIDs {
			session, active, err := s.gate.protectionSession(ctx, sessionID)
			if err != nil {
				return err
			}
			if !active {
				continue
			}
			if session.PendingOperations != 0 || session.UncertainRemoteResult || session.State != "protected" && session.State != "anchored" {
				return ErrRecoveryBarrier
			}
			owners, err := s.gate.ProtectionSessionOwners(ctx, sessionID)
			if err != nil {
				return err
			}
			if session.State == "protected" && !containsString(owners, task.ID) {
				if err := s.gate.AddProtectionOwnerDuringRecovery(ctx, sessionID, task.ID); err != nil {
					return err
				}
			}
			if scopeID == "" && !(legacyTask && sessionID == legacyGrandfatheredSessionID) {
				scopeID = session.ScopeID
			}
			sessionBytes := session.ReservedBytes
			if legacyTask && sessionID == legacyGrandfatheredSessionID {
				sessionBytes = task.LegacyProtectionBytes
				if sessionBytes == 0 {
					return errors.New("legacy task has no immutable grandfathered reservation")
				}
			}
			if sessionBytes > math.MaxInt64-reservedBytes {
				return errors.New("recovered protection reservation overflows")
			}
			reservedBytes += sessionBytes
			activeIDs = append(activeIDs, sessionID)
		}
		if len(activeIDs) == 0 {
			if legacyTask {
				return errors.New("legacy task has no active aggregated grandfathered protection session")
			}
			return s.tasks.ReconcileProtectionState(ctx, task.ID, "", nil, 0, 0)
		}
		if scopeID == "" && legacyTask {
			scopeID, err = protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
			if err != nil {
				return err
			}
		}
		if scopeID == "" {
			return errors.New("recovered protection task has no scope")
		}
		requestedBytes := reservedBytes
		if legacyTask && task.RequestedBytes > 0 {
			requestedBytes = task.RequestedBytes
		}
		return s.tasks.ReconcileProtectionState(ctx, task.ID, scopeID, activeIDs, requestedBytes, reservedBytes)
	}

	if !legacyTask || task.RootDigest == "" {
		if task.GCProtected || task.ScopeID != "" || task.ReservedBytes != 0 || task.RecoveryReason != "" {
			return s.tasks.ReconcileProtectionState(ctx, task.ID, "", nil, 0, 0)
		}
		return nil
	}
	return errors.New("legacy task has no aggregated grandfathered protection session")
}

func recoveryBlockReason(task tasks.Task, cause error) string {
	if errors.Is(cause, ErrRecoveryBarrier) {
		return "remote_result_unknown"
	}
	if task.ProtocolVersion < 2 && task.Kind == tasks.RootKind && task.Phase == "solidification" {
		if len(task.Checkpoints) == 0 {
			return "legacy_uncheckpointed_write"
		}
		if errors.Is(cause, ErrProtectionCapacity) {
			return "legacy_scope_over_limit"
		}
		if _, err := taskGraphBytes(task); err != nil {
			return "legacy_scope_unbounded"
		}
		return "legacy_scope_recovery_failed"
	}
	return "protection_state_unknown"
}

func uniqueSessionIDs(sessionIDs []string) []string {
	seen := make(map[string]struct{}, len(sessionIDs))
	result := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func taskGraphBytes(task tasks.Task) (int64, error) {
	var total int64
	for digest, object := range task.Graph {
		if digest == "" || object.Size < 0 || object.Size > math.MaxInt64-total {
			return 0, errors.New("legacy task graph cannot be safely reserved")
		}
		total += object.Size
	}
	if _, ok := task.Graph[task.RootDigest]; !ok {
		return 0, errors.New("legacy task graph is missing its root manifest")
	}
	return total, nil
}

func taskProtectionReservationBytes(task *tasks.Task, sessionID string, session protectionSessionRecord) (int64, error) {
	if sessionID == legacyGrandfatheredSessionID {
		if session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID || task.LegacyProtectionBytes < 1 {
			return 0, errors.New("legacy task reservation metadata is invalid")
		}
		return task.LegacyProtectionBytes, nil
	}
	return session.ReservedBytes, nil
}
