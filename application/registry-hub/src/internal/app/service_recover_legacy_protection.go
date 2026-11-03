package app

import (
	"context"
	"errors"
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
