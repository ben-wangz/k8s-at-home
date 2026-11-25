package historyretention

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"strings"
	"time"
)

func waitForCompletion(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, path string) (map[string]any, error) {
	deadline := time.NewTimer(90 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	lastOperation := map[string]any{}
	for {
		status, operation, err := call(ctx, environment, &credential, http.MethodGet, path, "")
		if err != nil || status != http.StatusOK {
			return nil, fmt.Errorf("read cleanup operation status=%d error=%v", status, err)
		}
		lastOperation = operation
		switch testkit.String(operation["status"]) {
		case "done", "completed":
			if testkit.Int(operation["failed"]) != 0 {
				return nil, fmt.Errorf("cleanup operation completed with failures: %v", operation)
			}
			return operation, nil
		case "failed":
			return nil, fmt.Errorf("cleanup operation failed: %v", operation)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			id := strings.TrimPrefix(path, "/api/v1/history/cleanup/")
			jobStatus, job, jobErr := call(ctx, environment, &credential, http.MethodGet, "/api/v1/background/history_cleanup/"+id, "")
			workerStatus, worker, workerErr := call(ctx, environment, &credential, http.MethodGet, "/api/v1/workers/history_cleanup", "")
			return nil, fmt.Errorf("cleanup operation did not finish within 90 seconds: operation=%v jobStatus=%d job=%v jobError=%v workerStatus=%d worker=%v workerError=%v", lastOperation, jobStatus, job, jobErr, workerStatus, worker, workerErr)
		case <-ticker.C:
		}
	}
}

func waitForRangePreview(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, path string) (map[string]any, error) {
	deadline := time.NewTimer(90 * time.Second)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		status, preview, err := call(ctx, environment, &credential, http.MethodGet, path, "")
		if err != nil || status != http.StatusOK {
			return nil, fmt.Errorf("read range cleanup preview status=%d error=%v", status, err)
		}
		switch testkit.String(preview["status"]) {
		case "completed":
			return preview, nil
		case "failed":
			return nil, fmt.Errorf("range cleanup preview did not complete: %v", preview)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("range cleanup preview did not complete within 90 seconds: %v", preview)
		case <-ticker.C:
		}
	}
}

func terminalHistoryProjectionWait(retention any) (time.Duration, error) {
	retentionSeconds := testkit.Int(retention)
	if retentionSeconds <= 30 {
		return 0, fmt.Errorf("terminal history retention must exceed the 30-second projection margin")
	}
	return time.Duration(retentionSeconds-30) * time.Second, nil
}

func waitForTerminalHistory(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, path string, timeout time.Duration) (map[string]any, error) {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	lastStatus := 0
	lastDetail := map[string]any{}
	for {
		status, detail, err := call(ctx, environment, &credential, http.MethodGet, path, "")
		if err != nil {
			return nil, err
		}
		lastStatus, lastDetail = status, detail
		if status == http.StatusOK {
			return detail, nil
		}
		if status != http.StatusServiceUnavailable {
			return nil, fmt.Errorf("terminal work history status=%d body=%v", status, detail)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			receiptStatus, receiptWorker, receiptErr := call(ctx, environment, &credential, http.MethodGet, "/api/v1/workers/history_receipts", "")
			workStatus, workWorker, workErr := call(ctx, environment, &credential, http.MethodGet, "/api/v1/workers/history_work", "")
			return nil, fmt.Errorf("terminal work history was not projected within %s: status=%d detail=%v history_receipts_status=%d worker=%v error=%v history_work_status=%d worker=%v error=%v", timeout, lastStatus, lastDetail, receiptStatus, receiptWorker, receiptErr, workStatus, workWorker, workErr)
		case <-ticker.C:
		}
	}
}

func waitForReceiptCleanup(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, timeout time.Duration) (int64, int64, error) {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		status, policy, err := call(ctx, environment, &credential, http.MethodGet, "/api/v1/history/retention", "")
		if err != nil || status != http.StatusOK {
			return 0, 0, fmt.Errorf("read automatic receipt cleanup status=%d error=%v", status, err)
		}
		workers := testkit.Map(policy["workers"])
		receipts := testkit.Int(testkit.Map(workers["receipt_cleanup"])["deleted"])
		archives := testkit.Int(testkit.Map(workers["diagnostic_retention_receipt_archives"])["deleted"])
		if receipts > 0 && archives > 0 {
			return receipts, archives, nil
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-deadline.C:
			return 0, 0, fmt.Errorf("automatic receipt and archive cleanup did not complete within %s: receipts=%d archives=%d", timeout, receipts, archives)
		case <-ticker.C:
		}
	}
}

func waitUntil(ctx context.Context, at time.Time) error {
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseTime(value any) (time.Time, error) {
	parsed, ok := value.(string)
	if !ok || parsed == "" {
		return time.Time{}, fmt.Errorf("missing timestamp")
	}
	return time.Parse(time.RFC3339Nano, parsed)
}

func withinDuration(start, end time.Time, minimum, maximum time.Duration) bool {
	if start.IsZero() || end.IsZero() {
		return false
	}
	duration := end.Sub(start)
	return duration >= minimum && duration <= maximum
}

func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func call(ctx context.Context, environment testkit.CaseContext, credential *testkit.BasicCredential, method, path, body string) (int, map[string]any, error) {
	return callWithHeaders(ctx, environment, credential, method, path, body, nil)
}

func callWithHeaders(ctx context.Context, environment testkit.CaseContext, credential *testkit.BasicCredential, method, path, body string, headers map[string]string) (int, map[string]any, error) {
	var requestBody io.Reader
	if body != "" {
		requestBody = strings.NewReader(body)
		if headers == nil {
			headers = map[string]string{}
		}
		headers["Content-Type"] = "application/json"
	}
	response, err := testkit.Request(ctx, testkit.NewHTTPClient(20*time.Second, nil), method, strings.TrimRight(environment.AdminURL, "/")+path, requestBody, headers, credential)
	if err != nil {
		return 0, nil, err
	}
	status := response.StatusCode
	var result map[string]any
	_, err = support.DecodeJSON(response, 2<<20, &result)
	return status, result, err
}
