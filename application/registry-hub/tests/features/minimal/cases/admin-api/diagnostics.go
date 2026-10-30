package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyDiagnostics(ctx context.Context, environment testkit.CaseContext, taskID string) (map[string]any, error) {
	credential, err := support.ReadAdminCredential(ctx, environment)
	if err != nil {
		return nil, err
	}
	client := testkit.NewHTTPClient(30*time.Second, nil)
	get := func(path string, authenticated bool) (*http.Response, error) {
		var basic *testkit.BasicCredential
		if authenticated {
			basic = &credential
		}
		return testkit.Request(ctx, client, http.MethodGet, strings.TrimRight(environment.AdminURL, "/")+path, nil, nil, basic)
	}
	details := map[string]any{}
	for _, path := range []string{"/api/v1/status", "/api/v1/workers", "/api/v1/events", "/api/v1/blob-intents", "/api/v1/transfers", "/api/v1/storage/summary"} {
		response, err := get(path, false)
		if err != nil {
			return nil, err
		}
		if err := testkit.ExpectStatus(response, http.StatusUnauthorized); err != nil {
			return nil, fmt.Errorf("diagnostic authentication %s: %w", path, err)
		}
	}
	for _, path := range []string{"/api/v1/status", "/api/v1/workers", "/api/v1/workers/storage_inventory", "/api/v1/events?limit=1", "/api/v1/blob-intents?limit=1", "/api/v1/transfers?limit=1", "/api/v1/download-reservations?limit=1", "/api/v1/tasks/" + url.PathEscape(taskID) + "/dependencies?limit=1"} {
		response, err := get(path, true)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, fmt.Errorf("diagnostic %s returned %s", path, response.Status)
		}
		var body map[string]any
		raw, err := support.DecodeJSON(response, 4<<20, &body)
		if err != nil {
			return nil, err
		}
		for _, private := range []string{"diagnosticOutbox", "internalPath", "Authorization", credential.Password} {
			if private != "" && strings.Contains(string(raw), private) {
				return nil, fmt.Errorf("diagnostic %s exposed private data", path)
			}
		}
		if strings.Contains(path, "limit=") || path == "/api/v1/workers" {
			items, ok := body["items"].([]any)
			if !ok || path == "/api/v1/workers" && len(items) == 0 {
				return nil, fmt.Errorf("diagnostic %s has no items array", path)
			}
		}
		if path == "/api/v1/status" && (testkit.String(body["instanceID"]) == "" || testkit.String(body["sourceCoverage"]) == "") {
			return nil, fmt.Errorf("system diagnostic has no instance or source coverage")
		}
		details[path] = "passed"
	}
	for _, path := range []string{"/api/v1/status?unexpected=true", "/api/v1/workers?unexpected=true", "/api/v1/events?unexpected=true", "/api/v1/blob-intents?unexpected=true", "/api/v1/storage/usage?dimension=invalid"} {
		response, err := get(path, true)
		if err != nil {
			return nil, err
		}
		if err := testkit.ExpectStatus(response, http.StatusBadRequest); err != nil {
			return nil, fmt.Errorf("diagnostic query validation %s: %w", path, err)
		}
	}
	storage, err := verifyStorageDiagnostics(get)
	if err != nil {
		return nil, err
	}
	details["storageReport"] = storage
	return details, nil
}

func verifyStorageDiagnostics(get func(string, bool) (*http.Response, error)) (string, error) {
	response, err := get("/api/v1/storage/summary", true)
	if err != nil {
		return "", err
	}
	status := response.StatusCode
	var summary map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &summary); err != nil {
		return "", err
	}
	if status == http.StatusServiceUnavailable {
		if summary["errors"] == nil {
			return "", fmt.Errorf("unavailable storage report has no structured reason")
		}
		return "not_available; accounting acceptance remains pending", nil
	}
	if status != http.StatusOK || testkit.String(summary["snapshotID"]) == "" || summary["domains"] == nil {
		return "", fmt.Errorf("invalid storage report: status=%d", status)
	}
	query := url.Values{"snapshotID": {testkit.String(summary["snapshotID"])}, "dimension": {"category"}, "limit": {"1"}}
	for pages := 0; pages < 3; pages++ {
		response, err := get("/api/v1/storage/usage?"+query.Encode(), true)
		if err != nil {
			return "", err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return "", fmt.Errorf("storage usage returned %s", response.Status)
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return "", err
		}
		if page["snapshotID"] != summary["snapshotID"] || page["coverage"] == nil || page["truncated"] == nil {
			return "", fmt.Errorf("storage usage lost snapshot identity or coverage")
		}
		if _, ok := page["items"].([]any); !ok {
			return "", fmt.Errorf("storage usage has no items array")
		}
		token := testkit.String(page["nextToken"])
		if token == "" {
			break
		}
		query.Set("token", token)
	}
	return "available; bounded snapshot pagination passed", nil
}
