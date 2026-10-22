package gc

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func taskDetailForBlobDigest(ctx context.Context, environment testkit.CaseContext, repository, digest string, since time.Time, excludedRound string) (map[string]any, error) {
	query := url.Values{
		"authority": {"github.com"}, "repository": {repositoryPath(repository)},
		"digest": {digest}, "since": {since.Add(-time.Second).Format(time.RFC3339)}, "limit": {"100"},
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["kind"]) != "blob" || testkit.String(item["blobDigest"]) != digest || excludedRound != "" && testkit.String(item["roundID"]) == excludedRound {
			continue
		}
		id := testkit.String(item["id"])
		return taskDetailByID(ctx, environment, id)
	}
	return nil, fmt.Errorf("no matching blob task for digest %s", digest)
}

func taskDetailForReference(ctx context.Context, environment testkit.CaseContext, repository, reference string) (map[string]any, error) {
	query := url.Values{"authority": {"github.com"}, "repository": {repository[strings.IndexByte(repository, '/')+1:]}, "digest": {reference}, "limit": {"100"}}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
	if err != nil {
		return nil, err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return nil, err
	}
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		if testkit.String(item["reference"]) != reference {
			continue
		}
		id := testkit.String(item["id"])
		detail, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(id), nil, nil)
		if err != nil {
			return nil, err
		}
		var result map[string]any
		if _, err := support.DecodeJSON(detail, 4<<20, &result); err != nil {
			return nil, err
		}
		return result, nil
	}
	return nil, fmt.Errorf("no matching task for reference %s", reference)
}

func hasQuiescentUnanchoredSession(task map[string]any) (bool, error) {
	for _, raw := range testkit.Slice(task["protectionSessions"]) {
		session := testkit.Map(raw)
		if session["mayContainUnanchored"] == true && testkit.Int(session["pendingOperations"]) == 0 && session["uncertainRemoteResult"] != true {
			return true, nil
		}
	}
	return false, nil
}

func rejectGCWhileSolidificationProtected(ctx context.Context, environment testkit.CaseContext) (bool, error) {
	before, err := gcStatus(ctx, environment)
	if err != nil {
		return false, err
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc", nil, nil)
	if err != nil {
		return false, err
	}
	if response.StatusCode != http.StatusConflict {
		if response.StatusCode == http.StatusAccepted {
			var started map[string]any
			if _, decodeErr := support.DecodeJSON(response, 2<<20, &started); decodeErr == nil {
				_, _ = waitForGC(ctx, environment)
			}
		} else {
			response.Body.Close()
		}
		return false, fmt.Errorf("GC during unanchored solidification returned %d, want 409", response.StatusCode)
	}
	var envelope struct {
		Errors []struct {
			Code   string `json:"code"`
			Detail struct {
				Blockers []any `json:"blockers"`
			} `json:"detail"`
		} `json:"errors"`
	}
	if _, err := support.DecodeJSON(response, 2<<20, &envelope); err != nil {
		return false, err
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "GC_BLOCKED" || len(envelope.Errors[0].Detail.Blockers) == 0 {
		return false, fmt.Errorf("GC conflict did not report solidification blockers")
	}
	after, err := gcStatus(ctx, environment)
	if err != nil {
		return false, err
	}
	if testkit.String(testkit.Map(after["gate"])["state"]) != "open" || testkit.String(testkit.Map(after["run"])["id"]) != testkit.String(testkit.Map(before["run"])["id"]) {
		return false, fmt.Errorf("rejected GC changed gate state or started a GC run")
	}
	return true, nil
}

func requireNoProtectionBlockers(status map[string]any) error {
	if testkit.String(testkit.Map(status["gate"])["state"]) != "open" {
		return fmt.Errorf("GC gate did not reopen after solidification cancellation")
	}
	for _, raw := range testkit.Slice(status["sessions"]) {
		session := testkit.Map(raw)
		state := testkit.String(session["state"])
		if state == "protected" || state == "anchoring" || state == "stop_requested" || state == "quiescing" || state == "quiesced" || state == "recovery_required" {
			return fmt.Errorf("solidification protection session %s remains in %s after cancellation", testkit.String(session["id"]), state)
		}
	}
	return nil
}

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
