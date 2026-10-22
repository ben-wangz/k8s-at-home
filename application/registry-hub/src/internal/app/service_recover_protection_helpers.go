package app

import (
	"errors"
	"math"
	"registry-hub/internal/tasks"
	"sort"
)

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
