package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestHistoryCleanupAPIEnforcesPermissionsPrincipalBindingAndIdempotency(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{
		{Username: "operator", Password: "secret", Permissions: []string{auth.HistoryCleanup, auth.Observe}},
		{Username: "cleaner", Password: "secret", Permissions: []string{auth.HistoryCleanup}},
		{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}},
	}}
	openAPI := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodGet, "/openapi.json", "", "")
	if openAPI.Code != http.StatusOK {
		t.Fatalf("read OpenAPI: status=%d body=%s", openAPI.Code, openAPI.Body.String())
	}
	var document map[string]any
	if err := json.Unmarshal(openAPI.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	enum, ok := historyCleanupOpenAPIEnum(document)
	if !ok || len(enum) != len(historyCleanupCategories) {
		t.Fatalf("history cleanup category enum missing from OpenAPI: %v", enum)
	}
	cutoff := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339Nano)
	body := `{"categories":["terminal_transfer_history"],"cutoff":"` + cutoff + `"}`
	first := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup/preview", "", body)
	if first.Code != http.StatusOK {
		t.Fatalf("create preview: status=%d body=%s", first.Code, first.Body.String())
	}
	var preview historyCleanupPreview
	if err := json.Unmarshal(first.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	second := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup/preview", "", body)
	var repeated historyCleanupPreview
	if second.Code != http.StatusOK || json.Unmarshal(second.Body.Bytes(), &repeated) != nil || repeated.ID == preview.ID {
		t.Fatalf("repeated preview: status=%d body=%s", second.Code, second.Body.String())
	}
	if got := historyCleanupHTTPRequest(t, f.service, "observer", http.MethodPost, "/api/v1/history/cleanup/preview", "", body); got.Code != http.StatusForbidden {
		t.Fatalf("observer preview permission status=%d body=%s", got.Code, got.Body.String())
	}
	if got := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup/preview", "", strings.TrimSuffix(body, "}")+`,"prefix":"diagnostics/"}`); got.Code != http.StatusBadRequest {
		t.Fatalf("preview accepted an unknown prefix field: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := historyCleanupHTTPRequest(t, f.service, "cleaner", http.MethodGet, "/api/v1/history/retention", "", ""); got.Code != http.StatusForbidden {
		t.Fatalf("cleanup permission unexpectedly reads retention: status=%d body=%s", got.Code, got.Body.String())
	}
	if got := historyCleanupHTTPRequest(t, f.service, "observer", http.MethodGet, "/api/v1/history/retention", "", ""); got.Code != http.StatusOK {
		t.Fatalf("observer retention status=%d body=%s", got.Code, got.Body.String())
	}
	if _, status, err := f.service.submitHistoryCleanup(context.Background(), "other", "foreign-request", historyCleanupExecutionRequest{PreviewID: preview.ID}); status != http.StatusNotFound || !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("foreign principal submitted preview: status=%d error=%v", status, err)
	}

	requestBody := `{"previewID":"` + preview.ID + `"}`
	accepted := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup", "cleanup-request-1", requestBody)
	var task historyCleanupTaskView
	if accepted.Code != http.StatusAccepted || json.Unmarshal(accepted.Body.Bytes(), &task) != nil || task.ID == "" {
		t.Fatalf("submit cleanup: status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	retry := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup", "cleanup-request-1", requestBody)
	var retryTask historyCleanupTaskView
	if retry.Code != http.StatusAccepted || json.Unmarshal(retry.Body.Bytes(), &retryTask) != nil || retryTask.ID != task.ID {
		t.Fatalf("idempotent retry: status=%d body=%s", retry.Code, retry.Body.String())
	}
	conflict := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup", "cleanup-request-1", `{"previewID":"`+repeated.ID+`"}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("reused key with different preview status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	policyPreview, err := f.service.createHistoryCleanupPreview(context.Background(), "operator", historyCleanupPreviewRequest{
		Categories: []string{"terminal_transfer_history"}, Cutoff: time.Now().UTC().Add(-96 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := inventory.Update(context.Background(), f.service.store, historyCleanupPreviewKey(policyPreview.ID), func(value *historyCleanupPreview) error {
		value.PolicyVersion = "retired-policy"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, status, err := f.service.submitHistoryCleanup(context.Background(), "operator", "policy-request", historyCleanupExecutionRequest{PreviewID: policyPreview.ID}); status != http.StatusConflict || err == nil {
		t.Fatalf("stale policy preview status=%d error=%v", status, err)
	}
	if err := inventory.Update(context.Background(), f.service.store, historyCleanupPreviewKey(preview.ID), func(value *historyCleanupPreview) error {
		value.ExpiresAt = time.Now().UTC().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, status, err := f.service.submitHistoryCleanup(context.Background(), "operator", "expired-request", historyCleanupExecutionRequest{PreviewID: preview.ID}); status != http.StatusGone || !errors.Is(err, inventory.ErrExpired) {
		t.Fatalf("expired preview status=%d error=%v", status, err)
	}
	if err := f.service.pruneHistoryCleanupPreviews(context.Background()); err != nil {
		t.Fatal(err)
	}
	expiredRetry := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodPost, "/api/v1/history/cleanup", "cleanup-request-1", requestBody)
	var expiredRetryTask historyCleanupTaskView
	if expiredRetry.Code != http.StatusAccepted || json.Unmarshal(expiredRetry.Body.Bytes(), &expiredRetryTask) != nil || expiredRetryTask.ID != task.ID {
		t.Fatalf("idempotent retry after preview expiry: status=%d body=%s", expiredRetry.Code, expiredRetry.Body.String())
	}
	localStatus := historyCleanupHTTPRequest(t, f.service, "operator", http.MethodGet, task.StatusURL, "", "")
	if localStatus.Code != http.StatusOK {
		t.Fatalf("status lookup after preview expiry: status=%d body=%s", localStatus.Code, localStatus.Body.String())
	}
	foreignStatus := historyCleanupHTTPRequest(t, f.service, "observer", http.MethodGet, task.StatusURL, "", "")
	if foreignStatus.Code != http.StatusNotFound {
		t.Fatalf("foreign status lookup: status=%d body=%s", foreignStatus.Code, foreignStatus.Body.String())
	}
}

func TestHistoryCleanupContinuesAcrossEmptyPrefixesAfterPreviewRemoval(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	date := time.Now().UTC().Add(-48 * time.Hour).Format("20060102")
	key := diagnostics.BatchPrefix + date + "/late-batch.json"
	body := `{"batch":"history-cleanup"}`
	if _, err := f.service.store.Put(ctx, key, strings.NewReader(body), int64(len(body)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.service.createHistoryCleanupPreview(ctx, "operator", historyCleanupPreviewRequest{
		Categories: []string{"diagnostic_events"}, Cutoff: cutoff,
	})
	if err != nil || preview.Counts["diagnostic_events"].Eligible != 1 {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	view, status, err := f.service.submitHistoryCleanup(ctx, "operator", "event-batch-request", historyCleanupExecutionRequest{PreviewID: preview.ID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit cleanup: status=%d error=%v", status, err)
	}
	if err := f.service.store.Delete(ctx, historyCleanupPreviewKey(preview.ID)); err != nil {
		t.Fatal(err)
	}
	job, err := f.service.backgroundQueue().Read(ctx, historyCleanupWorkKind, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	for range 12 {
		progress, done, stepErr := f.service.historyCleanupStep(ctx, &job)
		if stepErr != nil {
			t.Fatal(stepErr)
		}
		job.Progress, err = json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			completed = true
			break
		}
	}
	if !completed {
		t.Fatal("history cleanup did not finish all category prefixes")
	}
	if _, err := f.service.store.Head(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("event batch in second prefix remains: %v", err)
	}
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, f.service.store, historyCleanupOperationKey(view.ID))
	if err != nil || operation.Status != "completed" || operation.Counts["diagnostic_events"].Deleted != 1 {
		t.Fatalf("completed operation=%+v error=%v", operation, err)
	}
}

func TestHistoryCleanupRetryRepairsPersistedOperationBeforeQueueEnqueue(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	preview, err := f.service.createHistoryCleanupPreview(ctx, "operator", historyCleanupPreviewRequest{
		Categories: []string{"terminal_transfer_history"}, Cutoff: time.Now().UTC().Add(-72 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "recovery-request"
	requestHash := diagnostics.ID("history-cleanup-request", preview.ID)
	idempotencyID := diagnostics.ID("history-cleanup-idempotency", "operator", key)
	source := diagnostics.ID("history-cleanup-request", "operator", key, requestHash)
	created := time.Now().UTC()
	jobID := workqueue.ID(historyCleanupWorkKind, source)
	operation := historyCleanupOperation{
		SchemaVersion: 1, ID: jobID, PreviewID: preview.ID, Principal: "operator", Categories: preview.Categories,
		Cutoff: preview.Cutoff, PolicyVersion: preview.PolicyVersion, Status: "pending", Counts: map[string]historyCleanupCounts{},
		QueueSource: source, CreatedAt: created, IdempotencyID: idempotencyID,
	}
	if err := putHistoryCleanupJSON(ctx, f.service.store, historyCleanupOperationKey(jobID), operation, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	idempotency := historyCleanupIdempotency{SchemaVersion: 1, RequestID: requestHash, OperationID: jobID, ExpiresAt: operation.ExpiresAt}
	if err := putHistoryCleanupJSON(ctx, f.service.store, historyCleanupIdempotencyPrefix+idempotencyID+".json", idempotency, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	view, status, err := f.service.submitHistoryCleanup(ctx, "operator", key, historyCleanupExecutionRequest{PreviewID: preview.ID})
	if err != nil || status != http.StatusAccepted || view.ID != jobID {
		t.Fatalf("retry recovery: view=%+v status=%d error=%v", view, status, err)
	}
	if _, err := f.service.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobID); err != nil {
		t.Fatalf("missing queue job after idempotent retry: %v", err)
	}
}

func TestHistoryCleanupResultRetentionStartsWhenOperationCompletes(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	preview, err := f.service.createHistoryCleanupPreview(ctx, "operator", historyCleanupPreviewRequest{
		Categories: []string{"terminal_transfer_history"}, Cutoff: time.Now().UTC().Add(-72 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	view, status, err := f.service.submitHistoryCleanup(ctx, "operator", "completion-retention", historyCleanupExecutionRequest{PreviewID: preview.ID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit cleanup: status=%d error=%v", status, err)
	}
	operationID := view.ID
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, f.service.store, historyCleanupOperationKey(operationID))
	if err != nil || !operation.ExpiresAt.IsZero() {
		t.Fatalf("active operation expiry=%s error=%v", operation.ExpiresAt, err)
	}
	if err := f.service.saveHistoryCleanupOperation(ctx, operation, historyCleanupProgress{Counts: map[string]historyCleanupCounts{}}, true); err != nil {
		t.Fatal(err)
	}
	operation, _, err = inventory.Read[historyCleanupOperation](ctx, f.service.store, historyCleanupOperationKey(operationID))
	if err != nil || operation.Status != "completed" || !operation.ExpiresAt.Equal(operation.CompletedAt.Add(f.service.diagnosticsConfig().HistoryCleanupResultRetention)) {
		t.Fatalf("completed operation=%+v error=%v", operation, err)
	}
	idempotencyID := diagnostics.ID("history-cleanup-idempotency", "operator", "completion-retention")
	idempotency, _, err := inventory.Read[historyCleanupIdempotency](ctx, f.service.store, historyCleanupIdempotencyPrefix+idempotencyID+".json")
	if err != nil || !idempotency.ExpiresAt.Equal(operation.ExpiresAt) {
		t.Fatalf("idempotency expiry=%s operation expiry=%s error=%v", idempotency.ExpiresAt, operation.ExpiresAt, err)
	}
}

func TestHistoryCleanupViewUsesPersistedResultCompletionTime(t *testing.T) {
	completedAt := time.Date(2026, time.November, 22, 5, 0, 0, 0, time.UTC)
	jobCompletedAt := completedAt.Add(1200 * time.Millisecond)
	operation := historyCleanupOperation{
		ID: "cleanup-id", PreviewID: "preview-id", Status: "completed", CompletedAt: completedAt,
		ExpiresAt: completedAt.Add(30 * time.Second),
	}
	job := workqueue.Job{ID: operation.ID, State: "done", CreatedAt: completedAt.Add(-time.Second), CompletedAt: jobCompletedAt}

	view := historyCleanupViewFromJob(job, operation)
	if !view.CompletedAt.Equal(completedAt) || !view.ExpiresAt.Equal(view.CompletedAt.Add(30*time.Second)) {
		t.Fatalf("cleanup result timestamps completed=%s expires=%s; want operation completion %s and a 30-second window", view.CompletedAt, view.ExpiresAt, completedAt)
	}

	operation.CompletedAt = time.Time{}
	view = historyCleanupViewFromJob(job, operation)
	if !view.CompletedAt.Equal(jobCompletedAt) {
		t.Fatalf("legacy cleanup result completion=%s, want queue completion %s", view.CompletedAt, jobCompletedAt)
	}
}

func TestManualAndAutomaticHistoryCleanupTreatConcurrentDeleteAsMissing(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	now := time.Now().UTC()
	id := "history-cleanup-transfer"
	transferKey := diagnostics.Prefix + "transfers/" + id + ".json"
	record := terminalTransferHistoryRecord{
		SchemaVersion: 1, ID: id, SourceKey: transferKey, SourceVersion: "source-version", Revision: "revision-1",
		TerminalAt: now.Add(-72 * time.Hour), View: transferView{ID: id},
	}
	key := terminalTransferHistoryKey(id)
	if err := putHistoryCleanupJSON(ctx, f.service.store, key, record, storage.PutOptions{IfAbsent: true}); err != nil {
		t.Fatal(err)
	}
	preview, err := f.service.createHistoryCleanupPreview(ctx, "operator", historyCleanupPreviewRequest{
		Categories: []string{"terminal_transfer_history"}, Cutoff: now.Add(-24 * time.Hour),
	})
	if err != nil || preview.Counts["terminal_transfer_history"].Eligible != 1 {
		t.Fatalf("preview=%+v error=%v", preview, err)
	}
	jobView, status, err := f.service.submitHistoryCleanup(ctx, "operator", "overlap-request", historyCleanupExecutionRequest{PreviewID: preview.ID})
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("submit cleanup: status=%d error=%v", status, err)
	}
	entry, err := f.service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	var automatic sourceSweepCheckpoint
	if err := f.service.cleanupDiagnosticRetentionEntry(ctx, entry, &automatic); err != nil {
		t.Fatalf("automatic cleanup: %v", err)
	}
	if automatic.Status.Counts["deleted"] != 1 {
		t.Fatalf("automatic cleanup counters=%#v", automatic.Status.Counts)
	}
	job, err := f.service.backgroundQueue().Read(ctx, historyCleanupWorkKind, jobView.ID)
	if err != nil {
		t.Fatal(err)
	}
	job.Progress, err = json.Marshal(historyCleanupProgress{
		Cursor: inventory.Cursor{Entries: []storage.Entry{entry}}, Counts: map[string]historyCleanupCounts{},
	})
	if err != nil {
		t.Fatal(err)
	}
	progressValue, _, err := f.service.historyCleanupStep(ctx, &job)
	progress, ok := progressValue.(historyCleanupProgress)
	if err != nil || !ok || progress.Counts["terminal_transfer_history"].Missing != 1 {
		t.Fatalf("manual overlap progress=%+v type=%t error=%v", progressValue, ok, err)
	}
}

func historyCleanupHTTPRequest(t *testing.T, service *Service, user, method, path, idempotencyKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.SetBasicAuth(user, "secret")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	service.AdminHandler().ServeHTTP(response, request)
	return response
}

func historyCleanupOpenAPIEnum(document map[string]any) ([]any, bool) {
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		return nil, false
	}
	path, ok := paths["/api/v1/history/cleanup/preview"].(map[string]any)
	if !ok {
		return nil, false
	}
	post, ok := path["post"].(map[string]any)
	if !ok {
		return nil, false
	}
	requestBody, ok := post["requestBody"].(map[string]any)
	if !ok {
		return nil, false
	}
	content, ok := requestBody["content"].(map[string]any)
	if !ok {
		return nil, false
	}
	media, ok := content["application/json"].(map[string]any)
	if !ok {
		return nil, false
	}
	schema, ok := media["schema"].(map[string]any)
	if !ok {
		return nil, false
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil, false
	}
	categories, ok := properties["categories"].(map[string]any)
	if !ok {
		return nil, false
	}
	items, ok := categories["items"].(map[string]any)
	if !ok {
		return nil, false
	}
	enum, ok := items["enum"].([]any)
	return enum, ok
}
