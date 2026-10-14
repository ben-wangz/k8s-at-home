package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

type gcCompletionStore struct {
	storage.Store
	verifyOpenAfterState bool
	expectedOpenState    string
	failRunWrite         bool
	openedAfterState     bool
}

type interruptedGCJobClient struct {
	status    jobStatus
	err       error
	createErr error
}

func (c *interruptedGCJobClient) createJob(_ context.Context, job kubeJob) (jobStatus, error) {
	if c.createErr != nil {
		return jobStatus{}, c.createErr
	}
	metadata := job.Metadata
	return jobStatus{
		Metadata: struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		}{
			UID:         "gc-job-uid",
			Annotations: metadata["annotations"].(map[string]string),
			Labels:      metadata["labels"].(map[string]string),
		},
		Spec: job.Spec,
	}, nil
}

func (c *interruptedGCJobClient) getJob(context.Context, string) (jobStatus, error) {
	if c.err != nil {
		return jobStatus{}, c.err
	}
	return c.status, nil
}

func TestKubernetesGCStatusAPIInterruptionStaysUnknownAndWriteBlocked(t *testing.T) {
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
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		GCMode: "kubernetes", GCNamespace: "develop", GCImage: "registry:3.1.2",
		GCConfigMap: "registry-config", GCConfigKey: "config.yml",
	}
	client := &interruptedGCJobClient{err: errors.New("simulated Kubernetes API disconnect")}
	metrics := newMetrics()
	controller := &gcController{
		service: &Service{
			cfg: cfg, store: store, gate: gate, uploads: &uploadSessions{store: store},
			metrics: metrics, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		jobClient: client,
		run: gcRun{
			SchemaVersion: 1, ID: "0123456789abcdef0123456789abcdef", Mode: "kubernetes",
			State: "draining", ConfigDigest: gcJobConfigDigest(cfg),
		},
	}
	controller.wg.Add(1)
	controller.execute(ctx, controller.run.ID)
	if run := controller.Snapshot(); run.State != "failed_unknown" || run.ErrorClass != "executor_unknown" {
		t.Fatalf("GC run after API disconnect = (%q, %q), want failed_unknown/executor_unknown", run.State, run.ErrorClass)
	}
	if state := gate.State().State; state != "failed_unknown" {
		t.Fatalf("write gate after API disconnect = %q, want failed_unknown", state)
	}
	if _, err := gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("write admission after API disconnect = %v, want ErrWritesClosed", err)
	}
	if _, err := controller.Recover(ctx, false); err == nil {
		t.Fatal("GC recovery proceeded while the Kubernetes API remained unavailable")
	}
	if state := gate.State().State; state != "failed_unknown" {
		t.Fatalf("write gate after refused recovery = %q, want failed_unknown", state)
	}
	assertGCUnknownMetrics(t, metrics)
}

func TestKubernetesGCStatusAPIRejectedResponsesStayUnknownAndWriteBlocked(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "forbidden", err: errors.New("Kubernetes API returned 403 Forbidden")},
		{name: "job disappeared", err: errors.New("Kubernetes API returned 404 NotFound")},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			if err := gate.BeginDrain(ctx); err != nil {
				t.Fatal(err)
			}
			cfg := config.Config{
				GCMode: "kubernetes", GCNamespace: "develop", GCImage: "registry:3.1.2",
				GCConfigMap: "registry-config", GCConfigKey: "config.yml",
			}
			metrics := newMetrics()
			controller := &gcController{
				service: &Service{
					cfg: cfg, store: store, gate: gate, uploads: &uploadSessions{store: store},
					metrics: metrics, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				},
				jobClient: &interruptedGCJobClient{err: test.err},
				run: gcRun{
					SchemaVersion: 1, ID: "aabbccddeeff0011aabbccddeeff0011", Mode: "kubernetes",
					State: "draining", ConfigDigest: gcJobConfigDigest(cfg),
				},
			}
			controller.wg.Add(1)
			controller.execute(ctx, controller.run.ID)
			if run := controller.Snapshot(); run.State != "failed_unknown" || run.ErrorClass != "executor_unknown" {
				t.Fatalf("GC run after API status failure = (%q, %q), want failed_unknown/executor_unknown", run.State, run.ErrorClass)
			}
			if state := gate.State().State; state != "failed_unknown" {
				t.Fatalf("write gate after API status failure = %q, want failed_unknown", state)
			}
			if _, err := gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
				t.Fatalf("write admission after API status failure = %v, want ErrWritesClosed", err)
			}
			if _, err := controller.Recover(ctx, false); err == nil {
				t.Fatal("GC recovery proceeded while the status API returned an error")
			}
			assertGCUnknownMetrics(t, metrics)
		})
	}
}

func TestKubernetesGCCreateAPIInterruptionStaysUnknownAndWriteBlocked(t *testing.T) {
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
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		GCMode: "kubernetes", GCNamespace: "develop", GCImage: "registry:3.1.2",
		GCConfigMap: "registry-config", GCConfigKey: "config.yml",
	}
	metrics := newMetrics()
	controller := &gcController{
		service: &Service{
			cfg: cfg, store: store, gate: gate, uploads: &uploadSessions{store: store},
			metrics: metrics, logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		},
		jobClient: &interruptedGCJobClient{createErr: errors.New("simulated Kubernetes API create failure")},
		run: gcRun{
			SchemaVersion: 1, ID: "fedcba9876543210fedcba9876543210", Mode: "kubernetes",
			State: "draining", ConfigDigest: gcJobConfigDigest(cfg),
		},
	}
	controller.wg.Add(1)
	controller.execute(ctx, controller.run.ID)
	if run := controller.Snapshot(); run.State != "failed_unknown" || run.ErrorClass != "executor_unknown" {
		t.Fatalf("GC run after Job create failure = (%q, %q), want failed_unknown/executor_unknown", run.State, run.ErrorClass)
	}
	if state := gate.State().State; state != "failed_unknown" {
		t.Fatalf("write gate after Job create failure = %q, want failed_unknown", state)
	}
	if _, err := gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
		t.Fatalf("write admission after Job create failure = %v, want ErrWritesClosed", err)
	}
	assertGCUnknownMetrics(t, metrics)
}

func assertGCUnknownMetrics(t *testing.T, metrics *metrics) {
	t.Helper()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), `registry_hub_gc_runs_total{result="unknown"} 1`) ||
		!strings.Contains(response.Body.String(), "registry_hub_gc_duration_seconds_count 1") {
		t.Fatalf("GC unknown fault metrics are missing or malformed: %s", response.Body.String())
	}
}
