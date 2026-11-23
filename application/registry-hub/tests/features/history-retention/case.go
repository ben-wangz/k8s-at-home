package historyretention

import (
	"context"
	"embed"
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

//go:embed case.json
var metadata embed.FS

type cleanupPreview struct {
	ID            string         `json:"id"`
	Principal     string         `json:"principal"`
	Categories    []string       `json:"categories"`
	PolicyVersion string         `json:"policyVersion"`
	Counts        map[string]any `json:"counts"`
	Complete      bool           `json:"complete"`
	CreatedAt     time.Time      `json:"createdAt"`
	ExpiresAt     time.Time      `json:"expiresAt"`
}

type cleanupCredentials struct {
	Operator  testkit.BasicCredential
	Observer  testkit.BasicCredential
	TaskAdmin testkit.BasicCredential
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-history-retention")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	credentials, err := readCleanupCredentials(ctx, environment)
	if err != nil {
		return err
	}
	cutoff := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	previewBody, err := json.Marshal(map[string]any{
		"categories": []string{"terminal_transfer_history"},
		"cutoff":     cutoff,
	})
	if err != nil {
		return err
	}

	if status, _, err := call(ctx, environment, nil, http.MethodGet, "/api/v1/history/retention", ""); err != nil || status != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated retention read status=%d error=%v", status, err)
	}
	if status, _, err := call(ctx, environment, &credentials.TaskAdmin, http.MethodPost, "/api/v1/history/cleanup/preview", string(previewBody)); err != nil || status != http.StatusForbidden {
		return fmt.Errorf("task-admin preview status=%d error=%v", status, err)
	}
	status, retention, err := call(ctx, environment, &credentials.Observer, http.MethodGet, "/api/v1/history/retention", "")
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("read retention policy status=%d error=%v", status, err)
	}
	if testkit.String(retention["policyVersion"]) == "" || len(testkit.Map(retention["policies"])) != 4 {
		return fmt.Errorf("retention response is missing the effective policy: %v", retention)
	}
	retentionProfile := map[string]int64{
		"diagnosticEventRetention": 120, "diagnosticEventBudgetRetention": 180, "diagnosticCollectionRetention": 360,
		"retryRequestRetention": 180, "transferObservationRetention": 240,
		"transferExpiryRetention": 240, "terminalHistoryRetention": 240, "fullWorkReceiptRetention": 300,
		"cleanupPreviewTTL": 30, "cleanupResultRetention": 30, "historySweepInterval": 2, "historySweepMaxInterval": 5, "retentionSweepInterval": 10,
		"ordinaryEventFlushInterval": 5, "criticalEventFlushInterval": 2,
	}
	durations := testkit.Map(retention["durationsSeconds"])
	for name, expected := range retentionProfile {
		if testkit.Int(durations[name]) != expected {
			return fmt.Errorf("retention duration %s = %v, want %d seconds", name, durations[name], expected)
		}
	}
	if status, _, err := call(ctx, environment, &credentials.Observer, http.MethodGet, "/api/v1/workers/diagnostic_retention", ""); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("removed aggregate retention worker status=%d error=%v", status, err)
	}
	for _, kind := range []string{"diagnostic_retention_events", "diagnostic_retention_event_batches"} {
		if status, _, err := call(ctx, environment, &credentials.Observer, http.MethodGet, "/api/v1/workers/"+kind, ""); err != nil || status != http.StatusOK {
			return fmt.Errorf("active retention worker %s status=%d error=%v", kind, status, err)
		}
	}
	status, events, err := call(ctx, environment, &credentials.Observer, http.MethodGet, "/api/v1/events", "")
	if err != nil || status != http.StatusOK || testkit.Int(events["retentionSeconds"]) != 120 {
		return fmt.Errorf("event retention API status=%d retention=%v error=%v", status, events["retentionSeconds"], err)
	}
	if status, _, err := call(ctx, environment, &credentials.Observer, http.MethodPost, "/api/v1/history/cleanup/preview", string(previewBody)); err != nil || status != http.StatusForbidden {
		return fmt.Errorf("observer preview status=%d error=%v", status, err)
	}
	invalidBody := strings.TrimSuffix(string(previewBody), "}") + `,"prefix":"diagnostics/"}`
	if status, _, err := call(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup/preview", invalidBody); err != nil || status != http.StatusBadRequest {
		return fmt.Errorf("preview accepted a raw prefix status=%d error=%v", status, err)
	}

	first, err := createPreview(ctx, environment, credentials.Operator, string(previewBody))
	if err != nil {
		return err
	}
	second, err := createPreview(ctx, environment, credentials.Operator, string(previewBody))
	if err != nil {
		return err
	}
	if first.ID == second.ID || first.Principal != credentials.Operator.Username || !first.Complete ||
		first.PolicyVersion == "" || len(first.Categories) != 1 || first.Categories[0] != "terminal_transfer_history" || first.Counts["terminal_transfer_history"] == nil ||
		!withinDuration(first.CreatedAt, first.ExpiresAt, 29*time.Second, 30*time.Second+100*time.Millisecond) {
		return fmt.Errorf("cleanup preview lost identity, scope, or completeness: %+v", first)
	}

	requestBody, err := json.Marshal(map[string]string{"previewID": first.ID})
	if err != nil {
		return err
	}
	idempotencyKey := "history-retention-" + first.ID
	status, accepted, err := callWithHeaders(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup", string(requestBody), map[string]string{"Idempotency-Key": idempotencyKey})
	if err != nil || status != http.StatusAccepted {
		return fmt.Errorf("submit cleanup status=%d error=%v", status, err)
	}
	operationID := testkit.String(accepted["id"])
	statusURL := testkit.String(accepted["statusURL"])
	if operationID == "" || statusURL != "/api/v1/history/cleanup/"+operationID || testkit.String(accepted["previewID"]) != first.ID {
		return fmt.Errorf("cleanup submission has invalid durable identity: %v", accepted)
	}
	status, retried, err := callWithHeaders(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup", string(requestBody), map[string]string{"Idempotency-Key": idempotencyKey})
	if err != nil || status != http.StatusAccepted || testkit.String(retried["id"]) != operationID {
		return fmt.Errorf("idempotent cleanup retry status=%d body=%v error=%v", status, retried, err)
	}
	conflictBody, err := json.Marshal(map[string]string{"previewID": second.ID})
	if err != nil {
		return err
	}
	if status, _, err := callWithHeaders(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup", string(conflictBody), map[string]string{"Idempotency-Key": idempotencyKey}); err != nil || status != http.StatusConflict {
		return fmt.Errorf("conflicting idempotency-key reuse status=%d error=%v", status, err)
	}
	if status, _, err := call(ctx, environment, &credentials.Observer, http.MethodGet, statusURL, ""); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("foreign principal read cleanup status status=%d error=%v", status, err)
	}
	completed, err := waitForCompletion(ctx, environment, credentials.Operator, statusURL)
	if err != nil {
		return err
	}
	completedAt, err := parseTime(completed["completedAt"])
	if err != nil {
		return fmt.Errorf("cleanup result omitted its completion time: %w", err)
	}
	resultExpiresAt, err := parseTime(completed["expiresAt"])
	if err != nil || !withinDuration(completedAt, resultExpiresAt, 29*time.Second, 30*time.Second+100*time.Millisecond) {
		return fmt.Errorf("cleanup result expiry = %v, completion = %v, error = %v", completed["expiresAt"], completedAt, err)
	}
	projectionWait, err := terminalHistoryProjectionWait(durations["terminalHistoryRetention"])
	if err != nil {
		return err
	}
	terminalHistory, err := waitForTerminalHistory(ctx, environment, credentials.Observer, "/api/v1/background/history_cleanup/"+operationID, projectionWait)
	if err != nil {
		return err
	}
	terminalAt, err := parseTime(terminalHistory["completedAt"])
	if err != nil {
		return fmt.Errorf("terminal work history omitted its completion time: %w", err)
	}
	terminalHistoryExpiresAt := terminalAt.Add(time.Duration(retentionProfile["terminalHistoryRetention"]) * time.Second)
	expiredPreview, err := createPreview(ctx, environment, credentials.Operator, string(previewBody))
	if err != nil {
		return err
	}
	if !withinDuration(expiredPreview.CreatedAt, expiredPreview.ExpiresAt, 29*time.Second, 30*time.Second+100*time.Millisecond) {
		return fmt.Errorf("short-profile preview expiry = %s", expiredPreview.ExpiresAt.Sub(expiredPreview.CreatedAt))
	}
	expiresAt := maxTime(maxTime(resultExpiresAt, expiredPreview.ExpiresAt), terminalHistoryExpiresAt)
	if err := waitUntil(ctx, expiresAt.Add(300*time.Millisecond)); err != nil {
		return err
	}
	expiredRequest, err := json.Marshal(map[string]string{"previewID": expiredPreview.ID})
	if err != nil {
		return err
	}
	if status, _, err := callWithHeaders(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup", string(expiredRequest), map[string]string{"Idempotency-Key": "expired-preview-" + expiredPreview.ID}); err != nil || status != http.StatusGone {
		return fmt.Errorf("expired cleanup preview submission status=%d error=%v", status, err)
	}
	if status, _, err := call(ctx, environment, &credentials.Operator, http.MethodGet, statusURL, ""); err != nil || status != http.StatusGone {
		return fmt.Errorf("expired cleanup result status=%d error=%v", status, err)
	}
	if status, _, err := call(ctx, environment, &credentials.Observer, http.MethodGet, "/api/v1/background/history_cleanup/"+operationID, ""); err != nil || status != http.StatusGone {
		return fmt.Errorf("expired terminal history status=%d error=%v", status, err)
	}
	receiptTTLGap := time.Duration(retentionProfile["fullWorkReceiptRetention"]-retentionProfile["terminalHistoryRetention"]) * time.Second
	retentionSweep := time.Duration(retentionProfile["retentionSweepInterval"]) * time.Second
	receiptCleanupWait := receiptTTLGap + min(retentionSweep*4, time.Minute) + 30*time.Second
	receiptCleanup, err := waitForReceiptCleanup(ctx, environment, credentials.Observer, receiptCleanupWait)
	if err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"policyVersion":                first.PolicyVersion,
		"retentionProfile":             retentionProfile,
		"previewIDs":                   []string{first.ID, second.ID, expiredPreview.ID},
		"operationID":                  operationID,
		"operation":                    completed,
		"terminalHistoryAt":            terminalAt,
		"terminalHistoryExpires":       terminalHistoryExpiresAt,
		"historyProjectionWaitSeconds": int64(projectionWait / time.Second),
		"expiredPreviewStatus":         http.StatusGone,
		"expiredResultStatus":          http.StatusGone,
		"expiredHistoryStatus":         http.StatusGone,
		"receiptCleanupDeleted":        receiptCleanup,
	})
}

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
