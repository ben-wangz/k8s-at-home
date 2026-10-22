package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
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
