package historyretention

import (
	"context"

	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func readCleanupCredentials(ctx context.Context, environment testkit.CaseContext) (cleanupCredentials, error) {
	_, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+"-external-history-admin")
	if err != nil {
		return cleanupCredentials{}, err
	}
	var configuration support.AdminAuth
	if err := json.Unmarshal(data["config.json"], &configuration); err != nil {
		return cleanupCredentials{}, err
	}
	credentials := cleanupCredentials{}
	found := map[string]bool{}
	for _, item := range configuration.Basic {
		if item.Password == "" || item.Token != "" {
			return cleanupCredentials{}, fmt.Errorf("history test credential is malformed")
		}
		permissions := append([]string(nil), item.Permissions...)
		sort.Strings(permissions)
		credential := testkit.BasicCredential{Username: item.Username, Password: item.Password}
		switch item.Username {
		case "history-operator":
			if strings.Join(permissions, " ") != "history-cleanup observe" {
				return cleanupCredentials{}, fmt.Errorf("history operator permissions are invalid")
			}
			credentials.Operator = credential
		case "history-observer":
			if strings.Join(permissions, " ") != "observe" {
				return cleanupCredentials{}, fmt.Errorf("history observer permissions are invalid")
			}
			credentials.Observer = credential
		case "task-admin":
			if strings.Join(permissions, " ") != "task-admin" {
				return cleanupCredentials{}, fmt.Errorf("task admin permissions are invalid")
			}
			credentials.TaskAdmin = credential
		default:
			return cleanupCredentials{}, fmt.Errorf("unexpected history test principal %q", item.Username)
		}
		if found[item.Username] {
			return cleanupCredentials{}, fmt.Errorf("duplicate history test principal %q", item.Username)
		}
		found[item.Username] = true
	}
	if len(found) != 3 {
		return cleanupCredentials{}, fmt.Errorf("history test Secret does not contain all expected principals")
	}
	return credentials, nil
}

func createPreview(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, body string) (cleanupPreview, error) {
	status, response, err := call(ctx, environment, &credential, http.MethodPost, "/api/v1/history/cleanup/preview", body)
	if err != nil || status != http.StatusOK {
		return cleanupPreview{}, fmt.Errorf("create cleanup preview status=%d error=%v", status, err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return cleanupPreview{}, err
	}
	var preview cleanupPreview
	if err := json.Unmarshal(encoded, &preview); err != nil {
		return cleanupPreview{}, err
	}
	return preview, nil
}

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

func waitForReceiptCleanup(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, timeout time.Duration) (int64, error) {
	deadline := time.NewTimer(timeout)
	ticker := time.NewTicker(time.Second)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		status, worker, err := call(ctx, environment, &credential, http.MethodGet, "/api/v1/workers/receipt_cleanup", "")
		if err != nil || status != http.StatusOK {
			return 0, fmt.Errorf("read automatic receipt cleanup status=%d error=%v", status, err)
		}
		scan := testkit.Map(worker["scan"])
		counts := testkit.Map(scan["counts"])
		if deleted := testkit.Int(counts["deleted"]); deleted > 0 {
			return deleted, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-deadline.C:
			return 0, fmt.Errorf("automatic receipt cleanup did not delete a receipt within %s: %v", timeout, worker)
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
