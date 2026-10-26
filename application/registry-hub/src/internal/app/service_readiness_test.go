package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"registry-hub/internal/storage/filesystem"
)

func TestReadinessRequiresOpenGateAndHealthyDependencies(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{gate: gate}
	service.storageHealthy.Store(true)
	service.registryHealthy.Store(true)

	service.refreshReadiness()
	if !service.ready.Load() {
		t.Fatal("open gate with healthy dependencies is not ready")
	}
	for _, state := range []gateRecord{
		{State: "failed_unknown"},
		{State: "draining"},
		{State: "restore"},
		{State: "open", RecoveryBarrier: true},
		{State: "open", Exclusive: true},
	} {
		service.setReadinessForGate(state)
		if service.ready.Load() {
			t.Errorf("gate state %#v was considered ready", state)
		}
	}
	service.registryHealthy.Store(false)
	service.setReadinessForGate(gate.State())
	if service.ready.Load() {
		t.Fatal("unhealthy Distribution was considered ready")
	}
	service.registryHealthy.Store(true)
	service.storageHealthy.Store(false)
	service.setReadinessForGate(gate.State())
	if service.ready.Load() {
		t.Fatal("unhealthy storage was considered ready")
	}
}

func TestReadinessUpdatesOnGateChangesAndHealthzRemainsLive(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{gate: gate}
	service.storageHealthy.Store(true)
	service.registryHealthy.Store(true)
	watchCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.watchReadiness(watchCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitReadiness(t, service, true)
	if err := gate.SetGCState(ctx, "failed_unknown", "", "storage"); err != nil {
		t.Fatal(err)
	}
	waitReadiness(t, service, false)
	if err := gate.SetGCState(ctx, "open", "", ""); err != nil {
		t.Fatal(err)
	}
	waitReadiness(t, service, true)

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	service.SetReady(false)
	service.handleAdmin(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("/healthz returned %d, want %d", response.Code, http.StatusOK)
	}
	response = httptest.NewRecorder()
	service.handleAdmin(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while not ready returned %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	service.ctx, service.cancel = context.WithCancel(ctx)
	service.BeginShutdown()
	service.setReadinessForGate(gate.State())
	if service.ready.Load() {
		t.Fatal("shutdown service was marked ready after a gate update")
	}
}

func waitReadiness(t *testing.T, service *Service, want bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if service.ready.Load() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("readiness = %t, want %t", service.ready.Load(), want)
}
