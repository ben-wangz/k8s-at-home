package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func TestBackgroundManagementReadsAreProtectedAndReadOnly(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	q := workqueue.Queue{Store: f.service.store}
	job, err := q.Enqueue(ctx, "inventory", "broken", "payload", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Finish(ctx, &job, map[string]int{"phase": 1}, false, storage.ErrCorruptObject); err != nil {
		t.Fatal(err)
	}
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "operator", Password: "secret", Permissions: []string{auth.TaskAdmin}}, {Username: "gc", Password: "secret", Permissions: []string{auth.GCAdmin}}}}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store = guard
	f.service.inventory.Store = guard
	handler := f.service.AdminHandler()
	for _, path := range []string{"/api/v1/background/inventory", "/api/v1/background/inventory/" + job.ID, "/api/v1/images/status"} {
		for user, status := range map[string]int{"": 401, "gc": 403, "operator": 200} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if user != "" {
				r.SetBasicAuth(user, "secret")
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != status {
				t.Fatalf("GET %s as %s=%d body=%s", path, user, w.Code, w.Body.String())
			}
		}
	}
	if guard.writes != 0 {
		t.Fatalf("GET wrote %d records", guard.writes)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/background/inventory/"+job.ID+"/retry", nil)
	r.SetBasicAuth("operator", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("retry=%d body=%s", w.Code, w.Body.String())
	}
	current, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil || current.State != "pending" || current.ErrorClass != "corrupt_source" {
		t.Fatalf("retried=%+v err=%v", current, err)
	}
}

func TestHealthProbeContinuesWhileStatisticsAreBlocked(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, statsDone, probeDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	cursor := inventory.Cursor{}
	go func() {
		defer close(statsDone)
		f.service.runMetricSweep(ctx, "blocked", &cursor, func(ctx context.Context) (bool, error) { close(entered); <-ctx.Done(); return false, ctx.Err() }, func() { t.Error("incomplete aggregate was published") }, func() {})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("statistics did not start")
	}
	var healthy atomic.Bool
	var last, success atomic.Int64
	go func() {
		defer close(probeDone)
		f.service.runHealthProbe(ctx, func(context.Context) error { return nil }, &healthy, &last, &success)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for success.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !healthy.Load() || success.Load() == 0 {
		t.Error("statistics blocked health probing")
	}
	cancel()
	<-statsDone
	<-probeDone
	status := probeStatus(true, time.Now().Add(-2*time.Minute).UnixNano(), success.Load())
	if !status.Stale {
		t.Fatal("expired health result was reported as current")
	}
}
