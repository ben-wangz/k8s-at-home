package gc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func isProtectionBlockingState(state string) bool {
	switch state {
	case "protected", "anchoring", "stop_requested", "quiescing", "quiesced", "recovery_required":
		return true
	default:
		return false
	}
}

func summarizeProtectionSession(session map[string]any) map[string]any {
	fields := []string{
		"id", "scopeID", "reservationClass", "state", "ownerCount", "reservedBytes", "pendingOperations",
		"writePermitActive", "uncertainRemoteResult", "remoteRecoveryErrorClass", "remoteRecoveryNextAttemptAt",
		"mayContainUnanchored", "remoteOperationKind", "remoteDigest", "remoteSubject", "cancelPhase", "recoveryReason",
	}
	summary := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := session[field]; ok {
			summary[field] = value
		}
	}
	return summary
}

func protectionCleanupFailureEvidence(ctx context.Context, environment testkit.CaseContext, status, cancelled map[string]any, failure error) map[string]any {
	evidence := map[string]any{
		"failure": failure.Error(), "gate": summarizeGateStatus(testkit.Map(status["gate"])),
		"activeWrites": status["activeWrites"], "blockers": []map[string]any{}, "cancelledTasks": []map[string]any{},
	}
	blockers := make([]map[string]any, 0, 10)
	for _, raw := range testkit.Slice(status["sessions"]) {
		session := testkit.Map(raw)
		if !isProtectionBlockingState(testkit.String(session["state"])) {
			continue
		}
		id := testkit.String(session["id"])
		blocker := map[string]any{"session": summarizeProtectionSession(session)}
		owners, err := protectionSessionOwnerEvidence(ctx, environment, id)
		if err != nil {
			blocker["ownersError"] = err.Error()
		} else {
			blocker["owners"] = owners["owners"]
			blocker["ownersNextToken"] = owners["ownersNextToken"]
		}
		blockers = append(blockers, blocker)
		if len(blockers) == 10 {
			break
		}
	}
	evidence["blockers"] = blockers
	taskIDs, _ := cancelled["taskIDs"].([]string)
	tasks := make([]map[string]any, 0, min(len(taskIDs), 10))
	for _, id := range taskIDs {
		task, err := taskDetailByID(ctx, environment, id)
		if err != nil {
			tasks = append(tasks, map[string]any{"id": id, "error": err.Error()})
		} else {
			tasks = append(tasks, summarizeProtectionTask(task))
		}
		if len(tasks) == 10 {
			break
		}
	}
	evidence["cancelledTasks"] = tasks
	return evidence
}

func summarizeGateStatus(gate map[string]any) map[string]any {
	fields := []string{
		"state", "exclusive", "epoch", "recoveryBarrier", "recoveryReason", "protectionSessionCount",
		"newProtectionSessionCount", "reservedDistributionBytes", "legacyGrandfatheredBytes",
	}
	summary := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := gate[field]; ok {
			summary[field] = value
		}
	}
	return summary
}

func summarizeProtectionTask(task map[string]any) map[string]any {
	fields := []string{
		"id", "kind", "status", "cancelRequested", "cancelPhase", "terminalCleanupID", "terminalCleanupComplete",
		"gcProtected", "sessionIDs", "protectionSessionID", "protectionOwnerID", "recoveryReason", "updatedAt",
	}
	summary := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := task[field]; ok {
			summary[field] = value
		}
	}
	return summary
}

func protectionSessionOwnerEvidence(ctx context.Context, environment testkit.CaseContext, sessionID string) (map[string]any, error) {
	query := url.Values{"ownersFor": {sessionID}, "ownerLimit": {"100"}}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var status map[string]any
	if _, err := support.DecodeJSON(response, 1<<20, &status); err != nil {
		return nil, err
	}
	if testkit.String(status["ownerSessionID"]) != sessionID {
		return nil, errors.New("protection owner response does not match the requested session")
	}
	return map[string]any{"owners": status["owners"], "ownersNextToken": status["ownersNextToken"]}, nil
}

func protectionCleanupFailureMessage(ctx context.Context, environment testkit.CaseContext, status, cancelled map[string]any, failure error) string {
	evidence := protectionCleanupFailureEvidence(ctx, environment, status, cancelled, failure)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Sprintf("%v; failure evidence could not be encoded: %v", failure, err)
	}
	return string(encoded)
}
