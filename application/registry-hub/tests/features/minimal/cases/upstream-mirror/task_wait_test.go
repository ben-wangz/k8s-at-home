package upstreammirror

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTaskWaitAvoidsRepeatedHistoryScansAndReturnsFullTerminalDetails(t *testing.T) {
	listCalls, boundedCalls, fullCalls := 0, 0, 0
	read := func(path string) (map[string]any, error) {
		task := map[string]any{"id": "target", "reference": "image", "status": "running"}
		switch {
		case strings.HasPrefix(path, "/api/v1/tasks?"):
			listCalls++
			return map[string]any{"items": []any{task}}, nil
		case path == "/api/v1/tasks/target?graphLimit=1&ownerLimit=1":
			boundedCalls++
			task["status"] = "solidified"
			return task, nil
		case path == "/api/v1/tasks/target":
			fullCalls++
			task["status"], task["completeGraph"] = "solidified", true
			return task, nil
		default:
			t.Fatalf("unexpected task poll %q", path)
			return nil, nil
		}
	}
	result, err := waitTaskStatus(context.Background(), url.Values{"digest": {"image"}}, "image", "solidified", 5*time.Second, read)
	if err != nil || result["completeGraph"] != true || listCalls != 1 || boundedCalls != 1 || fullCalls != 1 {
		t.Fatalf("unexpected polling/result: lists=%d bounded=%d full=%d result=%v err=%v", listCalls, boundedCalls, fullCalls, result, err)
	}
}

func TestTaskWaitPreservesUnexpectedTerminalFailure(t *testing.T) {
	read := func(string) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "target", "reference": "image", "status": "failed", "errorClass": "storage"}}}, nil
	}
	_, err := waitTaskStatus(context.Background(), nil, "image", "solidified", time.Second, read)
	if err == nil || !strings.Contains(err.Error(), "ended in failed") {
		t.Fatalf("unexpected terminal task was accepted: %v", err)
	}
}

func TestTaskWaitTimeoutIncludesQuotaPredecessorEvidence(t *testing.T) {
	read := func(path string) (map[string]any, error) {
		if path != "/api/v1/tasks/predecessor?graphLimit=1&ownerLimit=1" {
			t.Fatalf("unexpected evidence read %q", path)
		}
		return map[string]any{
			"id": "predecessor", "reference": "older-image", "status": "waiting_capacity",
			"waitReason": "distribution_quota", "quotaWait": map[string]any{"category": "capacity", "coverage": "current_gate_and_demand"},
		}, nil
	}
	task := map[string]any{
		"id": "target", "reference": "image", "status": "waiting_capacity", "waitReason": "distribution_quota",
		"quotaWait": map[string]any{"category": "queue_predecessor", "predecessorID": "predecessor"},
	}
	err := taskWaitTimeoutError(read, "image", "solidified", 5*time.Minute, "waiting_capacity", task)
	if err == nil || !strings.Contains(err.Error(), `"category":"queue_predecessor"`) || !strings.Contains(err.Error(), `"predecessorTask"`) || !strings.Contains(err.Error(), `"status":"waiting_capacity"`) {
		t.Fatalf("timeout omitted quota predecessor evidence: %v", err)
	}
}

func TestTaskWaitTimeoutIncludesDependencyBlockers(t *testing.T) {
	dependencyPath := "/api/v1/tasks/target/dependencies?limit=32"
	read := func(path string) (map[string]any, error) {
		if path != dependencyPath {
			t.Fatalf("unexpected dependency evidence read %q", path)
		}
		return map[string]any{
			"blockingInPage":          1,
			"knownMissingBytesInPage": float64(4096),
			"totalScope":              "page; continue until nextToken is absent",
			"items": []any{map[string]any{
				"digest": "sha256:missing", "role": "blob", "declaredBytes": float64(4096),
				"cacheState": "missing", "registryState": "unknown", "reason": "waiting_external_payload",
				"nextAction": "client_pull_required", "taskStatus": "waiting_dependencies",
			}},
			"nextToken": "next-page",
		}, nil
	}
	task := map[string]any{
		"id": "target", "kind": "root", "reference": "image", "status": "waiting_dependencies",
		"waitReason": "dependencies_unavailable",
	}
	err := taskWaitTimeoutError(read, "image", "solidified", 5*time.Minute, "waiting_dependencies", task)
	for _, evidence := range []string{"\"kind\":\"root\"", "\"dependencyEvidence\"", "\"blockingInPage\":1", "sha256:missing", "waiting_external_payload", "\"hasMore\":true"} {
		if err == nil || !strings.Contains(err.Error(), evidence) {
			t.Fatalf("timeout omitted dependency evidence %q: %v", evidence, err)
		}
	}
}

func TestTaskWaitFindsTargetBeyondFilteredEmptyPage(t *testing.T) {
	calls := 0
	read := func(path string) (map[string]any, error) {
		calls++
		if calls == 1 {
			return map[string]any{"items": []any{}, "nextToken": "next/page"}, nil
		}
		task := map[string]any{"id": "target", "reference": "image", "status": "waiting_dependencies"}
		if calls == 2 {
			if !strings.Contains(path, "token=next%2Fpage") {
				t.Fatalf("task discovery did not follow pagination: %s", path)
			}
			return map[string]any{"items": []any{task}}, nil
		}
		if path != "/api/v1/tasks/target" {
			t.Fatalf("unexpected terminal detail request: %s", path)
		}
		return task, nil
	}
	_, err := waitTaskStatus(context.Background(), nil, "image", "waiting_dependencies", time.Second, read)
	if err != nil || calls != 3 {
		t.Fatalf("paginated discovery failed: calls=%d error=%v", calls, err)
	}
}
