package adminapi

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "admin-api")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	unsafe := environment.DependencyResults["security-defaults"]
	taskID, err := support.DetailString(unsafe, "taskID")
	if err != nil {
		return err
	}
	referrers := environment.DependencyResults["referrers-read"]
	repository, err := support.DetailString(referrers, "repository")
	if err != nil {
		return err
	}
	subject, err := support.DetailString(referrers, "subjectDigest")
	if err != nil {
		return err
	}
	client := testkit.NewHTTPClient(20*time.Second, nil)
	for _, path := range []string{"/metrics", "/docs", "/openapi.yaml", "/api/v1/tasks"} {
		response, requestErr := testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+path, nil, nil, nil)
		if requestErr != nil {
			return requestErr
		}
		if response.StatusCode != http.StatusUnauthorized {
			response.Body.Close()
			return fmt.Errorf("unauthenticated admin endpoint %s returned %s", path, response.Status)
		}
		response.Body.Close()
	}

	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?limit=1", nil, nil)
	if err != nil {
		return err
	}
	var page map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
		return err
	}
	if _, ok := page["items"].([]any); !ok {
		return fmt.Errorf("task list has no items array")
	}

	response, err = support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(taskID), nil, nil)
	if err != nil {
		return err
	}
	var task map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &task); err != nil {
		return err
	}
	if testkit.String(task["id"]) != taskID || testkit.String(task["status"]) != "failed" || testkit.String(task["authority"]) == "" || testkit.String(task["repository"]) == "" {
		return fmt.Errorf("SSRF task details are incomplete")
	}
	if strings.Contains(fmt.Sprint(task), "_state") || strings.Contains(fmt.Sprint(task), "password") {
		return fmt.Errorf("task details contain internal or credential material")
	}

	for _, operation := range []string{"retry", "cancel"} {
		response, err = support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/unknown-registry-hub/"+operation, nil, nil)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusNotFound {
			response.Body.Close()
			return fmt.Errorf("unknown task %s returned %s", operation, response.Status)
		}
		response.Body.Close()
	}

	query := url.Values{"repository": {repository}, "subject": {subject}, "limit": {"1"}}
	response, err = support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/referrers/reconcile?"+query.Encode(), nil, nil)
	if err != nil {
		return err
	}
	var reconcile map[string]any
	if _, err := support.DecodeJSON(response, 4<<20, &reconcile); err != nil {
		return err
	}
	if testkit.String(reconcile["repository"]) != repository || testkit.String(reconcile["subject"]) != subject {
		return fmt.Errorf("referrer reconcile response lost its identity")
	}
	invalidReconcile := []byte(`{"repository":"` + repository + `","subject":"` + subject + `","action":"replace","referrers":[]}`)
	response, err = support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/referrers/reconcile", bytes.NewReader(invalidReconcile), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusBadRequest {
		response.Body.Close()
		return fmt.Errorf("incomplete referrer replace returned %s", response.Status)
	}
	response.Body.Close()

	response, err = support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/gc", nil, nil)
	if err != nil {
		return err
	}
	var gc map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &gc); err != nil {
		return err
	}
	if state := testkit.String(testkit.Map(gc["gate"])["state"]); state != "open" {
		return fmt.Errorf("GC gate is %q, want open", state)
	}
	response, err = support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc", nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusConflict {
		response.Body.Close()
		return fmt.Errorf("disabled GC trigger returned %s", response.Status)
	}
	response.Body.Close()
	for _, path := range []string{"/api/v1/gc/retry", "/api/v1/gc/recover"} {
		response, err = support.AdminRequest(ctx, environment, http.MethodPost, path, bytes.NewReader([]byte(`{"confirmedNoExecutor":false}`)), map[string]string{"Content-Type": "application/json"})
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusConflict {
			response.Body.Close()
			return fmt.Errorf("GC operation %s returned %s", path, response.Status)
		}
		response.Body.Close()
	}

	response, err = support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/cleanup", nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return fmt.Errorf("task cleanup returned %s", response.Status)
	}
	response.Body.Close()
	registry := support.NewRegistryClient(environment)
	if _, _, err := support.ReadManifest(ctx, registry, repository, subject, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("cleanup removed the referrer subject: %w", err)
	}
	fixtureRepo, fixtureDigest, err := persistenceFixture(environment.DependencyResults)
	if err != nil {
		return err
	}
	if _, _, err := support.ReadManifest(ctx, registry, fixtureRepo, fixtureDigest, support.OCIManifestMediaType); err != nil {
		return fmt.Errorf("cleanup removed retained fixture: %w", err)
	}
	return support.WriteResult(environment, map[string]any{"taskID": taskID, "taskStatus": testkit.String(task["status"]), "gcGate": "open", "repository": fixtureRepo, "manifestDigest": fixtureDigest})
}

func persistenceFixture(results map[string]testkit.CaseResultFile) (string, string, error) {
	artifact := results["referrers-read"]
	repository, err := support.DetailString(artifact, "repository")
	if err != nil {
		return "", "", err
	}
	subject, err := support.DetailString(artifact, "subjectDigest")
	if err != nil {
		return "", "", err
	}
	return repository, subject, nil
}
