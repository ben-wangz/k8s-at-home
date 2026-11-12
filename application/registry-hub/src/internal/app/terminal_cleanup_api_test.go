package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/workqueue"
)

func TestTerminalReceiptAPIAndNullableSchema(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1024)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	q := workqueue.Queue{Store: f.service.store, BuildFinalSummary: buildBackgroundFinalSummary}
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "terminal_cleanup", "api-summary", terminalCleanupWork{TaskID: "task", CleanupID: "cleanup", RoundID: "round", Generation: 2}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	p := terminalCleanupProgress{Processed: 83, Yields: 6, State: protectionRecoveryCheckpoint{Step: "done"}}
	if err = q.Finish(ctx, &job, p, true, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/background/terminal_cleanup/" + job.ID, "/api/v1/background/terminal_cleanup?state=done&taskID=task"} {
		response := diagnosticGet(t, f.service, path)
		if response.Code != 200 {
			t.Fatalf("receipt API: %d %s", response.Code, response.Body.String())
		}
		var value map[string]any
		if err = json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(path, "?") {
			items, ok := value["items"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("receipt list omitted completion: %v", value)
			}
			value = items[0].(map[string]any)
		}
		summary := value["finalSummary"].(map[string]any)
		cleanup := value["terminalCleanup"].(map[string]any)
		if summary["processedSteps"] != float64(83) || cleanup["processed"] != float64(83) || cleanup["rootDemands"] != nil || value["payload"] != nil || value["progress"] != nil {
			t.Fatalf("receipt contract: %v", value)
		}
	}
	response := diagnosticGet(t, f.service, "/openapi.json")
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &spec) != nil {
		t.Fatal("missing OpenAPI")
	}
	found := false
	for _, schema := range spec.Components.Schemas {
		if schema.Properties["cursorUnit"] == nil || schema.Properties["processed"] == nil {
			continue
		}
		found = true
		encoded, _ := json.Marshal(schema.Properties["processed"])
		if !strings.Contains(string(encoded), "null") {
			t.Fatalf("missing counter schema nullability: %s", encoded)
		}
	}
	if !found {
		t.Fatal("missing terminal cleanup schema")
	}
}
