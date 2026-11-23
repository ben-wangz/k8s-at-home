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
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
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
