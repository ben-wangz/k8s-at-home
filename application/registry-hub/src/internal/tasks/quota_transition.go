package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func markQuotaTransition(task *Task) bool {
	if task.QuotaWakeRevision == 0 && task.QuotaWaitAt.IsZero() && task.Status != WaitingQuota && len(task.SessionIDs) == 0 && task.ProtectionSessionID == "" && task.ReservedBytes == 0 && task.ProtectedCacheBytes == 0 {
		return false
	}
	data, _ := json.Marshal([]any{task.Generation, task.RoundID, task.Status, task.WaitReason, task.QuotaWaitAt, task.QuotaWaitReason, task.ProtectionSessionID, task.SessionIDs, task.ReservedBytes, task.ProtectedCacheBytes, task.CancelRequested, task.TerminalCleanupComplete})
	hash := sha256.Sum256(data)
	state := hex.EncodeToString(hash[:])
	if state == task.QuotaWakeState {
		return false
	}
	task.QuotaWakeRevision++
	task.QuotaWakeState = state
	return true
}

func (m *Manager) notifyQuotaTransition(ctx context.Context, task Task, changed bool) {
	if handler, ok := m.processor.(TaskQuotaChangedHandler); ok && changed {
		handler.TaskQuotaChanged(ctx, task)
	}
}
