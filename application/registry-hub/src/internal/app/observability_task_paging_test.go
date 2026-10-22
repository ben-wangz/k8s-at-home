package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestTaskMetricsAggregatePagesAndRetainCompleteSnapshotOnFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newStagedAcceptanceFixture(t, 1<<20)
	store := &taskMetricsPagedStore{Store: f.service.store, failed: make(chan struct{}, 1)}
	manager, err := tasks.NewManager(store, 1, 100, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer manager.Close()
	f.service.tasks = manager
	for i, state := range []tasks.State{tasks.Pending, tasks.RetryWait, tasks.WaitingDeps, tasks.WaitingQuota, tasks.Failed} {
		root, _, err := manager.EnqueueResolved(ctx, "ghcr.io", "private/repo", fmt.Sprint(i), artifact.CalculateSHA256([]byte(fmt.Sprint(i))), ociManifestMediaType, 0)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		root.Status, root.RetryCount, root.WaitReason = state, i, "dependencies_unavailable"
		if state == tasks.WaitingQuota {
			root.WaitReason = "distribution_bytes"
		}
		saveLegacyAcceptanceTask(t, f, &root)
	}
	store.pages.Store(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.refreshMetrics(ctx)
	}()
	defer func() { cancel(); <-done }()
	samples := []string{
		`registry_hub_tasks{state="pending"} 1`,
		`registry_hub_tasks{state="retry_wait"} 1`,
		`registry_hub_tasks_by_kind_state{kind="root",state="waiting_dependencies"} 1`,
		`registry_hub_task_retry_attempts{kind="root",state="failed"} 4`,
		`registry_hub_task_waiting{reason="dependencies_unavailable"} 1`,
	}
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for !taskMetricsContain(f.service, samples) {
		select {
		case <-deadline.C:
			t.Fatal("task metrics did not aggregate all three pages")
		case <-ticker.C:
		}
	}
	if pages := store.pages.Load(); pages != 3 {
		t.Fatalf("task snapshot pages = %d, want 3", pages)
	}
	store.failLater.Store(true)
	select {
	case <-store.failed:
	case <-time.After(20 * time.Second):
		t.Fatal("later-page metrics failure was not reached")
	}
	if !taskMetricsContain(f.service, samples) {
		t.Fatal("partial task scan replaced the last complete snapshot")
	}
}

func taskMetricsContain(service *Service, samples []string) bool {
	response := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, sample := range samples {
		if !strings.Contains(response.Body.String(), sample) {
			return false
		}
	}
	return !strings.Contains(response.Body.String(), "private/repo")
}

type taskMetricsPagedStore struct {
	storage.Store
	pages     atomic.Int32
	failLater atomic.Bool
	failed    chan struct{}
}

func (s *taskMetricsPagedStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if prefix == "tasks/state/" {
		s.pages.Add(1)
		if s.failLater.Load() && token != "" {
			select {
			case s.failed <- struct{}{}:
			default:
			}
			return nil, "", storage.ErrUnavailable
		}
		limit = min(limit, 2)
	}
	return s.Store.List(ctx, prefix, token, limit)
}
