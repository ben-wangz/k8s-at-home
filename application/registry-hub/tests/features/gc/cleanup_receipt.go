package gc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func waitForCleanupReceipt(ctx context.Context, environment testkit.CaseContext, taskID, cleanupID string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		query := url.Values{"state": {"done"}, "taskID": {taskID}, "limit": {"32"}}
		for pages := 0; pages < 100; pages++ {
			path := "/api/v1/background/terminal_cleanup?" + query.Encode()
			response, err := retryCleanupReceiptUnavailable(ctx, func(requestCtx context.Context) (*http.Response, error) {
				return support.AdminRequest(requestCtx, environment, http.MethodGet, path, nil, nil)
			})
			if err != nil {
				return nil, err
			}
			if response.StatusCode != http.StatusOK {
				response.Body.Close()
				return nil, fmt.Errorf("list cleanup receipts returned %d", response.StatusCode)
			}
			var page map[string]any
			if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
				return nil, err
			}
			for _, raw := range testkit.Slice(page["items"]) {
				job := testkit.Map(raw)
				summary := testkit.Map(job["finalSummary"])
				if testkit.String(summary["cleanupID"]) != cleanupID {
					continue
				}
				return verifyCleanupReceipt(ctx, environment, job)
			}
			token := testkit.String(page["nextToken"])
			if token == "" {
				break
			}
			query.Set("token", token)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cleanup %s has no completed summary: %w", cleanupID, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func retryCleanupReceiptUnavailable(ctx context.Context, request func(context.Context) (*http.Response, error)) (*http.Response, error) {
	for {
		response, err := request(ctx)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusServiceUnavailable {
			return response, nil
		}
		response.Body.Close()
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("cleanup receipt endpoint remained unavailable (503): %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func verifyCleanupReceipt(ctx context.Context, environment testkit.CaseContext, listed map[string]any) (map[string]any, error) {
	path := testkit.String(listed["statusURL"])
	response, err := retryCleanupReceiptUnavailable(ctx, func(requestCtx context.Context) (*http.Response, error) {
		return support.AdminRequest(requestCtx, environment, http.MethodGet, path, nil, nil)
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("read cleanup receipt returned %d", response.StatusCode)
	}
	var detail map[string]any
	raw, err := support.DecodeJSON(response, 4<<20, &detail)
	if err != nil {
		return nil, err
	}
	summary := testkit.Map(detail["finalSummary"])
	left, _ := json.Marshal(listed["finalSummary"])
	right, _ := json.Marshal(summary)
	cleanup := testkit.Map(detail["terminalCleanup"])
	if string(left) != string(right) || testkit.Int(summary["schemaVersion"]) != 1 || testkit.String(summary["phase"]) == "" || testkit.String(summary["outcome"]) == "" || summary["processedSteps"] != cleanup["processed"] || summary["yields"] != cleanup["yields"] || summary["counterUnit"] != "state_machine_steps" || summary["coverage"] != "persisted_cleanup_execution" {
		return nil, fmt.Errorf("cleanup receipt lost counters, units, or list/detail parity: %s", raw)
	}
	for _, key := range []string{"firstExecutedAt", "completedAt"} {
		at, err := time.Parse(time.RFC3339Nano, testkit.String(summary[key]))
		if err != nil || at.IsZero() {
			return nil, fmt.Errorf("cleanup receipt has no %s", key)
		}
	}
	for _, private := range []string{"demandCursor", "validationLedger", "pendingMutation", "ownerSetID", "diagnosticOutbox"} {
		if strings.Contains(string(raw), private) {
			return nil, fmt.Errorf("cleanup receipt exposed %s", private)
		}
	}
	if detail["payload"] != nil || detail["progress"] != nil || len(right) > 8<<10 {
		return nil, fmt.Errorf("cleanup receipt exposed execution bodies or exceeded summary bound")
	}
	return summary, nil
}

func verifyCleanupSummariesAfterRestart(ctx context.Context, environment testkit.CaseContext, cancelled map[string]any) error {
	if err := restartBackendPod(ctx, environment); err != nil {
		return err
	}
	for _, item := range cancelled["cleanupSummaries"].([]map[string]any) {
		summary, err := waitForCleanupReceipt(ctx, environment, testkit.String(item["taskID"]), testkit.String(item["cleanupID"]))
		if err != nil {
			return err
		}
		before, _ := json.Marshal(item["summary"])
		after, _ := json.Marshal(summary)
		if string(before) != string(after) {
			return fmt.Errorf("cleanup summary changed after Backend restart")
		}
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var blocked error
	for {
		status, err := gcStatus(bounded, environment)
		blocked = err
		if blocked == nil {
			blocked = requireNoProtectionBlockers(status)
		}
		if blocked == nil && testkit.Map(status["gate"])["recoveryBarrier"] != true {
			cancelled["cleanupSummaryRestartVerified"] = true
			return nil
		}
		select {
		case <-bounded.Done():
			diagnosticCtx, diagnosticCancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer diagnosticCancel()
			failure := errors.Join(blocked, bounded.Err())
			return fmt.Errorf("restart did not restore a recovered gate without protection blockers: %w; failure evidence: %s", failure, protectionCleanupFailureMessage(diagnosticCtx, environment, status, cancelled, failure))
		case <-time.After(500 * time.Millisecond):
		}
	}
}
