package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/tasks"
)

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
