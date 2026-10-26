package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestReconcileNoRunHandlesStorageFailuresAndConditionalWrites(t *testing.T) {
	for _, test := range []struct {
		name        string
		configure   func(*reconcileFaultStore)
		wantUnknown bool
		wantOpen    bool
	}{
		{name: "transient coordinator read", configure: func(store *reconcileFaultStore) { store.failGateRead = true }, wantUnknown: true},
		{name: "stale conditional write", configure: func(store *reconcileFaultStore) { store.rejectOpen = true }, wantUnknown: true},
		{name: "unknown commit confirmed by exact readback", configure: func(store *reconcileFaultStore) { store.unknownOpen = true }, wantOpen: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			store := &reconcileFaultStore{Store: base}
			gate := newReconcileFailedUnknownGate(t, ctx, store)
			controller := &gcController{service: &Service{cfg: config.Config{GCMode: "disabled"}, store: store, gate: gate}}
			test.configure(store)
			_, err = controller.ReconcileNoRun(ctx, true)
			if test.wantOpen {
				if err != nil || gate.State().State != "open" {
					t.Fatalf("ReconcileNoRun() = %v, gate state %q; want confirmed open", err, gate.State().State)
				}
				return
			}
			if err == nil {
				t.Fatal("ReconcileNoRun() succeeded despite storage or conditional-write failure")
			}
			if test.name == "stale conditional write" && !errors.Is(err, errGCReconciliationConflict) {
				t.Fatalf("stale write error = %v, want a reconciliation conflict", err)
			}
			if test.wantUnknown && gate.State().State != "failed_unknown" {
				t.Fatalf("gate state after refused reconciliation = %q, want failed_unknown", gate.State().State)
			}
		})
	}
}

func TestAdminGCReconcileWakesGateBlockedTasksAfterDurableOpen(t *testing.T) {
	ctx := context.Background()
	service, manager, _ := newTerminalDemandFixture(t)
	root := newDemandRootTask(t, ctx, manager, "gc-reconcile")
	if err := saveDemandRootStatus(ctx, manager, root.ID, tasks.WaitingDeps, "gc_admission"); err != nil {
		t.Fatal(err)
	}
	if err := service.gate.SetGCState(ctx, "failed_unknown", "", "storage"); err != nil {
		t.Fatal(err)
	}
	service.cfg = config.Config{GCMode: "disabled", AdminAuth: config.Auth{Basic: []config.Credential{{
		Username: "operator", Password: "secret", Permissions: []string{auth.GCAdmin},
	}}}}
	service.gc = &gcController{service: service}
	service.logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	request := httptest.NewRequest(http.MethodPost, "/api/v1/gc/reconcile", strings.NewReader(`{"confirmedNoExecutor":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth("operator", "secret")
	response := httptest.NewRecorder()
	service.handleAdmin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GC reconciliation returned %d: %s", response.Code, response.Body.String())
	}
	current, err := manager.Get(ctx, root.ID)
	if err != nil || current.Status != tasks.Pending || current.WaitReason != "" {
		t.Fatalf("gate-blocked task after reconciliation = %#v err=%v, want pending", current, err)
	}
}
