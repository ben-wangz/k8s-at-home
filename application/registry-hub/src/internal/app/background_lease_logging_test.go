package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/lease"
	"strings"
	"testing"
	"time"
)

func TestExpectedBackgroundLeaseWaitDoesNotLogWarnings(t *testing.T) {
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewTextHandler(&output, nil))}
	for count := 0; count < 10; count++ {
		service.backgroundEvent("execution", "lease_wait", fmt.Errorf("leader: %w", lease.ErrHeld))
	}
	if output.Len() != 0 {
		t.Fatalf("expected lease contention produced warnings: %s", output.String())
	}
}

func TestExpectedBackgroundAdmissionYieldIsObservedWithoutWarning(t *testing.T) {
	var output bytes.Buffer
	service := &Service{logger: slog.New(slog.NewTextHandler(&output, nil))}
	service.backgroundEvent("diagnostic_retention_events", "retry_index_admission_yield", errBackgroundAdmissionYield)
	if output.Len() != 0 {
		t.Fatalf("expected admission yield produced warnings: %s", output.String())
	}
	state := service.workerStates["diagnostic_retention_events"]
	if state.AdmissionYields != 1 || state.LastError != nil {
		t.Fatalf("admission yield was recorded as failure or lost: %+v", state)
	}
}

func TestRetryIndexAdmissionYieldUsesExpectedEventWithoutWarning(t *testing.T) {
	var output bytes.Buffer
	service := &Service{
		logger:          slog.New(slog.NewTextHandler(&output, nil)),
		metrics:         newMetrics(),
		backgroundSlots: make(chan struct{}, 1),
	}
	service.backgroundSlots <- struct{}{}

	nextAt, nextBackoff := service.runSourceRetryIndexRepair(context.Background(), lease.Lease{}, "diagnostic_retention_events", &sourceSweepCheckpoint{}, time.Second)
	if nextAt.IsZero() || nextBackoff != 2*time.Second {
		t.Fatalf("admission yield retry schedule = (%s, %s)", nextAt, nextBackoff)
	}
	if output.Len() != 0 {
		t.Fatalf("expected admission yield produced warnings: %s", output.String())
	}
	state := service.workerStates["diagnostic_retention_events"]
	if state.AdmissionYields != 1 || state.LastError != nil {
		t.Fatalf("admission yield was recorded as failure or lost: %+v", state)
	}

	response := httptest.NewRecorder()
	service.metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := response.Body.String()
	if !strings.Contains(body, `registry_hub_events_total{event="background_diagnostic_retention_events_retry_index_admission_yield"} 1`) {
		t.Fatalf("expected retry-index admission-yield metric, got: %s", body)
	}
	if strings.Contains(body, `event="background_diagnostic_retention_events_retry_index_repair_failed"`) {
		t.Fatalf("admission yield was also recorded as repair failure: %s", body)
	}
}
