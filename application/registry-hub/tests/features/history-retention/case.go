package historyretention

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
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
	if testkit.String(retention["policyVersion"]) == "" || len(testkit.Map(retention["policies"])) != 7 {
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
	rangePreviewID, rangePreview, rangeOperationID, err := exerciseRangeCleanup(ctx, environment, credentials.Operator, credentials.Observer)
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
	expiredPreviewStatus, _, err := callWithHeaders(ctx, environment, &credentials.Operator, http.MethodPost, "/api/v1/history/cleanup", string(expiredRequest), map[string]string{"Idempotency-Key": "expired-preview-" + expiredPreview.ID})
	if err != nil || expiredPreviewStatus != http.StatusGone && expiredPreviewStatus != http.StatusNotFound {
		return fmt.Errorf("expired cleanup preview submission status=%d error=%v", expiredPreviewStatus, err)
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
	receiptCleanup, archiveCleanup, err := waitForReceiptCleanup(ctx, environment, credentials.Observer, receiptCleanupWait)
	if err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"policyVersion":                first.PolicyVersion,
		"retentionProfile":             retentionProfile,
		"previewIDs":                   []string{first.ID, second.ID, expiredPreview.ID},
		"rangePreviewID":               rangePreviewID,
		"rangePreviewStatus":           rangePreview["status"],
		"rangeOperationID":             rangeOperationID,
		"operationID":                  operationID,
		"operation":                    completed,
		"terminalHistoryAt":            terminalAt,
		"terminalHistoryExpires":       terminalHistoryExpiresAt,
		"historyProjectionWaitSeconds": int64(projectionWait / time.Second),
		"expiredPreviewStatus":         expiredPreviewStatus,
		"expiredResultStatus":          http.StatusGone,
		"expiredHistoryStatus":         http.StatusGone,
		"receiptCleanupDeleted":        receiptCleanup,
		"receiptArchiveCleanupDeleted": archiveCleanup,
	})
}
