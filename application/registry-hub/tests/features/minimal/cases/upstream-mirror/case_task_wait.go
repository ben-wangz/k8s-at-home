package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"time"
)

func waitForTaskStatusWithin(ctx context.Context, environment testkit.CaseContext, authority, repository, reference, expectedStatus string, timeout time.Duration) (map[string]any, error) {
	query := url.Values{
		"authority":  {authority},
		"repository": {repository},
		"digest":     {reference},
		"limit":      {"100"},
	}
	return waitTaskStatus(ctx, query, reference, expectedStatus, timeout, func(path string) (map[string]any, error) {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
		if err != nil {
			return nil, err
		}
		var document map[string]any
		_, err = support.DecodeJSON(response, 4<<20, &document)
		return document, err
	})
}

func waitTaskStatus(ctx context.Context, query url.Values, reference, expectedStatus string, timeout time.Duration, read func(string) (map[string]any, error)) (map[string]any, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	lastStatus, id, token := "not_observed", "", ""
	var lastTask map[string]any
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, taskWaitTimeoutError(read, reference, expectedStatus, timeout, lastStatus, lastTask)
		default:
		}
		path := "/api/v1/tasks?" + query.Encode()
		if token != "" {
			path += "&token=" + url.QueryEscape(token)
		}
		if id != "" {
			path = "/api/v1/tasks/" + url.PathEscape(id) + "?graphLimit=1&ownerLimit=1"
		}
		page, err := read(path)
		if err != nil {
			return nil, err
		}
		items := testkit.Slice(page["items"])
		if id != "" {
			items = []any{page}
		}
		for _, item := range items {
			task := testkit.Map(item)
			if testkit.String(task["reference"]) != reference {
				continue
			}
			id = testkit.String(task["id"])
			if id == "" {
				return nil, fmt.Errorf("upstream import task %q has no ID", reference)
			}
			lastStatus = testkit.String(task["status"])
			lastTask = task
			if lastStatus == expectedStatus {
				return read("/api/v1/tasks/" + url.PathEscape(id))
			}
			if lastStatus == "failed" || lastStatus == "cancelled" || lastStatus == "solidified" {
				return nil, fmt.Errorf("upstream import task %q ended in %s, want %s (%s; task evidence %s)", reference, lastStatus, expectedStatus, testkit.String(task["errorClass"]), taskWaitEvidenceJSON(task))
			}
		}
		if id == "" {
			token = testkit.String(page["nextToken"])
			if token != "" {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, taskWaitTimeoutError(read, reference, expectedStatus, timeout, lastStatus, lastTask)
		case <-time.After(time.Second):
		}
	}
}

func taskWaitTimeoutError(read func(string) (map[string]any, error), reference, expectedStatus string, timeout time.Duration, lastStatus string, task map[string]any) error {
	evidence := testkit.Map(taskWaitEvidence(task))
	quotaWait := testkit.Map(task["quotaWait"])
	predecessorID := testkit.String(quotaWait["predecessorID"])
	if predecessorID != "" {
		predecessor, err := read("/api/v1/tasks/" + url.PathEscape(predecessorID) + "?graphLimit=1&ownerLimit=1")
		if err != nil {
			evidence["predecessorReadError"] = err.Error()
		} else {
			evidence["predecessorTask"] = taskWaitEvidence(predecessor)
		}
	}
	if taskID := testkit.String(task["id"]); taskID != "" && testkit.String(task["waitReason"]) == "dependencies_unavailable" {
		path := "/api/v1/tasks/" + url.PathEscape(taskID) + "/dependencies?limit=32"
		dependencies, err := read(path)
		if err != nil {
			evidence["dependencyReadError"] = err.Error()
		} else {
			evidence["dependencyEvidence"] = taskDependencyWaitEvidence(dependencies)
		}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("upstream import task %q did not reach %s within %s (last status %s; task evidence unavailable: %v)", reference, expectedStatus, timeout, lastStatus, err)
	}
	return fmt.Errorf("upstream import task %q did not reach %s within %s (last status %s; task evidence %s)", reference, expectedStatus, timeout, lastStatus, encoded)
}

func taskWaitEvidence(task map[string]any) map[string]any {
	evidence := make(map[string]any)
	for _, key := range []string{"id", "kind", "reference", "status", "waitReason", "errorClass", "retryable", "retryCount", "quotaWaitAt", "quotaWaitReason", "quotaWaitBytes", "quotaReplaceableBytes", "quotaReplaceableSlots"} {
		if value, found := task[key]; found {
			evidence[key] = value
		}
	}
	if quotaWait := testkit.Map(task["quotaWait"]); len(quotaWait) > 0 {
		evidence["quotaWait"] = quotaWait
	}
	return evidence
}

func taskDependencyWaitEvidence(page map[string]any) map[string]any {
	evidence := map[string]any{
		"blockingInPage":          page["blockingInPage"],
		"knownMissingBytesInPage": page["knownMissingBytesInPage"],
		"totalScope":              page["totalScope"],
		"hasMore":                 testkit.String(page["nextToken"]) != "",
	}
	fields := []string{"digest", "role", "declaredBytes", "cacheState", "registryState", "intentID", "taskID", "reason", "nextAction", "taskStatus", "taskWaitReason", "waitResourceKey", "readErrors"}
	items := make([]map[string]any, 0, len(testkit.Slice(page["items"])))
	for _, raw := range testkit.Slice(page["items"]) {
		item := testkit.Map(raw)
		bounded := make(map[string]any)
		for _, field := range fields {
			if value, found := item[field]; found {
				bounded[field] = value
			}
		}
		items = append(items, bounded)
	}
	evidence["items"] = items
	return evidence
}

func taskWaitEvidenceJSON(task map[string]any) string {
	encoded, err := json.Marshal(taskWaitEvidence(task))
	if err != nil {
		return "{}"
	}
	return string(encoded)
}
