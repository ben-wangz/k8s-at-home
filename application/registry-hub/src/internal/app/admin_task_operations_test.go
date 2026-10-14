package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestAdminTaskRetryAndCancelSuccessPaths(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	service := &Service{
		cfg: config.Config{AdminAuth: config.Auth{Basic: []config.Credential{{
			Username: "operator", Password: "secret", Permissions: []string{auth.TaskAdmin},
		}}}},
		tasks: manager,
	}

	retryTask, _, err := manager.Enqueue(ctx, "ghcr.io", "team/retry", "latest")
	if err != nil {
		t.Fatal(err)
	}
	retryTask.Status = tasks.Failed
	retryTask.RootDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	retryTask.Retryable = true
	retryTask.ErrorClass = "upstream"
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+retryTask.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &retryTask, taskLease); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, taskLease); err != nil {
		t.Fatal(err)
	}
	retryResponse := requestAdminTaskOperation(t, service, "/api/v1/tasks/"+retryTask.ID+"/retry")
	if retryResponse.Code != http.StatusAccepted {
		t.Fatalf("task retry returned %d: %s", retryResponse.Code, retryResponse.Body.String())
	}
	var retried struct {
		ID     string      `json:"id"`
		Status tasks.State `json:"status"`
	}
	if err := json.Unmarshal(retryResponse.Body.Bytes(), &retried); err != nil {
		t.Fatal(err)
	}
	if retried.ID != retryTask.ID || retried.Status != tasks.Pending {
		t.Fatalf("retry response = %#v, want task %s pending", retried, retryTask.ID)
	}

	cancelTask, _, err := manager.Enqueue(ctx, "ghcr.io", "team/cancel", "latest")
	if err != nil {
		t.Fatal(err)
	}
	cancelResponse := requestAdminTaskOperation(t, service, "/api/v1/tasks/"+cancelTask.ID+"/cancel")
	if cancelResponse.Code != http.StatusAccepted {
		t.Fatalf("task cancellation returned %d: %s", cancelResponse.Code, cancelResponse.Body.String())
	}
	persisted, err := manager.Get(ctx, cancelTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != tasks.Cancelled || !persisted.CancelRequested {
		t.Fatalf("persisted cancelled task = %#v", persisted)
	}
}

func requestAdminTaskOperation(t *testing.T, service *Service, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, nil)
	request.SetBasicAuth("operator", "secret")
	response := httptest.NewRecorder()
	service.handleAdmin(response, request)
	return response
}
