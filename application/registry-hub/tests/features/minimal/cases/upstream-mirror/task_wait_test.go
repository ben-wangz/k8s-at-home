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
