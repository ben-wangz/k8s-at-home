package gc

import (
	"bytes"
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"time"
)

func waitForGCUnknown(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
		if err != nil {
			return nil, err
		}
		var status map[string]any
		if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
			return nil, err
		}
		run := testkit.Map(status["run"])
		switch state := testkit.String(run["state"]); state {
		case "failed_unknown":
			if testkit.String(run["errorClass"]) != "executor_unknown" || testkit.String(testkit.Map(status["gate"])["state"]) != "failed_unknown" {
				return nil, fmt.Errorf("GC identity failure did not preserve the write gate: run=%q gate=%q errorClass=%q",
					state, testkit.String(testkit.Map(status["gate"])["state"]), testkit.String(run["errorClass"]))
			}
			run["_gateState"] = "failed_unknown"
			return run, nil
		case "failed", "succeeded":
			return nil, fmt.Errorf("GC run ended in unexpected state %s", state)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("GC run did not enter failed_unknown before the case deadline: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func waitForGCJobSuccess(ctx context.Context, environment testkit.CaseContext, name string) error {
	for {
		job, err := testkit.KubectlGet(ctx, environment.Namespace, "job", name)
		if err != nil {
			return err
		}
		status := testkit.Map(job["status"])
		if testkit.Int(status["succeeded"]) > 0 {
			return nil
		}
		if testkit.Int(status["failed"]) > 0 {
			return fmt.Errorf("GC Job %s failed before recovery", name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("GC Job %s did not complete before recovery: %w", name, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func verifyGCRecoveryRemainsLocked(ctx context.Context, environment testkit.CaseContext, expectedID string) error {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc/recover", bytes.NewReader([]byte(`{"confirmedNoExecutor":false}`)), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	statusCode := response.StatusCode
	response.Body.Close()
	if statusCode != http.StatusConflict {
		return fmt.Errorf("GC recovery with mismatched Job identity returned status %d, want 409", statusCode)
	}
	statusResponse, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
	if err != nil {
		return err
	}
	var status map[string]any
	if _, err := support.DecodeJSON(statusResponse, 2<<20, &status); err != nil {
		return err
	}
	if testkit.String(testkit.Map(status["run"])["id"]) != expectedID || testkit.String(testkit.Map(status["run"])["state"]) != "failed_unknown" ||
		testkit.String(testkit.Map(status["gate"])["state"]) != "failed_unknown" {
		return fmt.Errorf("rejected GC recovery changed the unresolved run or opened its write gate")
	}
	return nil
}

func recoverGC(ctx context.Context, environment testkit.CaseContext, expectedID string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc/recover", bytes.NewReader([]byte(`{"confirmedNoExecutor":true}`)), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("GC recovery returned %s", response.Status)
	}
	var status map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
		return nil, err
	}
	run := testkit.Map(status["run"])
	gateState := testkit.String(testkit.Map(status["gate"])["state"])
	if testkit.String(run["id"]) != expectedID || testkit.String(run["state"]) != "recovered" || gateState != "open" {
		return nil, fmt.Errorf("GC recovery returned run=%q state=%q gate=%q", testkit.String(run["id"]), testkit.String(run["state"]), gateState)
	}
	run["_gateState"] = gateState
	return run, nil
}

func waitForGCFailure(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
		if err != nil {
			return nil, err
		}
		var status map[string]any
		if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
			return nil, err
		}
		run := testkit.Map(status["run"])
		switch state := testkit.String(run["state"]); state {
		case "failed":
			if testkit.String(run["errorClass"]) != "executor_failed" || testkit.String(testkit.Map(status["gate"])["state"]) != "failed" {
				return nil, fmt.Errorf("GC failure state does not preserve the write gate: run=%q gate=%q errorClass=%q",
					state, testkit.String(testkit.Map(status["gate"])["state"]), testkit.String(run["errorClass"]))
			}
			run["_gateState"] = "failed"
			return run, nil
		case "failed_unknown", "succeeded":
			return nil, fmt.Errorf("GC run ended in unexpected state %s", state)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("GC run did not fail before the case deadline: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

func verifyFailedGCJob(ctx context.Context, environment testkit.CaseContext, started, failed map[string]any) (map[string]any, error) {
	name := testkit.String(failed["jobName"])
	if name == "" || testkit.String(failed["id"]) != testkit.String(started["id"]) {
		return nil, fmt.Errorf("failed GC run identity changed while executing")
	}
	job, err := testkit.KubectlGet(ctx, environment.Namespace, "job", name)
	if err != nil {
		return nil, err
	}
	metadata := testkit.Map(job["metadata"])
	status := testkit.Map(job["status"])
	uid := testkit.String(metadata["uid"])
	if uid == "" || uid != testkit.String(failed["jobUID"]) || testkit.Int(status["failed"]) != 1 || testkit.Int(status["succeeded"]) != 0 ||
		testkit.Int(testkit.Map(job["spec"])["backoffLimit"]) != 0 {
		return nil, fmt.Errorf("GC Job did not record the expected single failed attempt")
	}
	pods, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods", "-l", "job-name="+name)
	if err != nil {
		return nil, err
	}
	if len(pods) != 1 || !hasJobOwner(testkit.Slice(testkit.Map(pods[0].Raw["metadata"])["ownerReferences"]), name) {
		return nil, fmt.Errorf("failed GC Job created %d owned Pods, want exactly one", len(pods))
	}
	return map[string]any{"name": name, "uid": uid, "podName": pods[0].Name, "podCount": len(pods)}, nil
}

func waitForGC(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
		if err != nil {
			return nil, err
		}
		var status map[string]any
		if _, err := support.DecodeJSON(response, 2<<20, &status); err != nil {
			return nil, err
		}
		run := testkit.Map(status["run"])
		switch state := testkit.String(run["state"]); state {
		case "succeeded":
			if testkit.String(testkit.Map(status["gate"])["state"]) != "open" {
				return nil, fmt.Errorf("GC succeeded without reopening the write gate")
			}
			run["_gateState"] = testkit.String(testkit.Map(status["gate"])["state"])
			return run, nil
		case "failed", "failed_unknown":
			return nil, fmt.Errorf("GC run ended in state %s", state)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("GC run did not finish before the case deadline: %w", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
