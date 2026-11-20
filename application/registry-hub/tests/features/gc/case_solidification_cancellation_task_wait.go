package gc

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func cancelAndWaitForTask(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := requestTaskCancellation(ctx, environment, taskID)
	if err != nil {
		return nil, err
	}
	result, err := waitForCancelledTask(ctx, environment, taskID)
	if err != nil {
		return nil, err
	}
	result["cancelResponseStatus"] = int64(testkit.Int(response["status"]))
	summary, err := waitForCleanupReceipt(ctx, environment, taskID, testkit.String(result["terminalCleanupID"]))
	if err != nil {
		return nil, err
	}
	result["verifiedCleanupSummary"] = summary
	return result, nil
}

func requestTaskCancellation(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/"+url.PathEscape(taskID)+"/cancel", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("cancel task %s returned %d, want 202 or an already-quiesced 200", taskID, response.StatusCode)
	}
	var task map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &task); err != nil {
		return nil, err
	}
	if response.StatusCode == http.StatusOK && (testkit.String(task["status"]) != "cancelled" || task["terminalCleanupComplete"] != true || task["gcProtectionActive"] == true) {
		return nil, fmt.Errorf("cancel task %s returned 200 without a terminal, unprotected state: %#v", taskID, task)
	}
	return map[string]any{
		"taskID": taskID, "status": int64(response.StatusCode), "responseTaskStatus": task["status"],
		"responseTerminalCleanupComplete": task["terminalCleanupComplete"], "responseGCProtectionActive": task["gcProtectionActive"],
	}, nil
}

func waitForCancelledTask(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		current, err := taskDetailByID(ctx, environment, taskID)
		if err != nil {
			return nil, err
		}
		if testkit.String(current["status"]) == "cancelled" && current["terminalCleanupComplete"] == true && current["gcProtectionActive"] != true {
			return current, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("task %s did not quiesce after cancellation", taskID)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
