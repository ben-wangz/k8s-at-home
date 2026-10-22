package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestGCFailureRestartRecoveryInvalidatesLocalObservation(t *testing.T) {
	for _, failureState := range []string{"failed", "failed_unknown"} {
		t.Run(failureState, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			f.service.ctx = ctx
			f.service.cfg.GCMode = "local"
			f.service.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			body := []byte("recheck after an uncertain GC execution")
			blob := contentRef{digest: artifact.CalculateSHA256(body), size: int64(len(body))}
			var reads atomic.Int32
			registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/" {
					w.WriteHeader(http.StatusOK)
					return
				}
				reads.Add(1)
				w.Header().Set("Docker-Content-Digest", blob.digest)
				_, _ = w.Write(body)
			}))
			f.service.registry, f.processor.registry = registry, registry
			task := &tasks.Task{ID: strings.Repeat("a", 32), Authority: "ghcr.io", Repository: "team/image"}
			path := "/v2/local/blobs/" + blob.digest
			if err := f.processor.verifyOrReuseLocalBlob(ctx, task, blob, task.ID, path); err != nil {
				t.Fatal(err)
			}
			oldEpoch := f.service.gate.State().Epoch
			if err := f.service.gate.BeginDrain(ctx); err != nil {
				t.Fatal(err)
			}
			controller := &gcController{service: f.service, run: gcRun{SchemaVersion: 1, ID: strings.Repeat("b", 32), Mode: "local", State: failureState}}
			controller.fail("executor_failed", failureState)
			gate, err := newWriteGate(ctx, f.service.store)
			if err != nil {
				t.Fatal(err)
			}
			f.service.gate, f.processor.gate = gate, gate
			controller, err = newGCController(f.service)
			if err != nil {
				t.Fatal(err)
			}
			if failureState == "failed" {
				epoch := gate.State().Epoch
				if err := gate.BeginRetry(ctx); err != nil {
					t.Fatal(err)
				}
				if gate.State().Epoch != epoch+1 {
					t.Fatal("GC retry did not invalidate its predecessor's epoch")
				}
				if err := gate.SetGCState(ctx, "failed", "local:retry", "executor_failed"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := gate.Enter(ctx); !errors.Is(err, ErrWritesClosed) {
				t.Fatalf("write after restart of %s GC = %v", failureState, err)
			}
			if _, err := controller.Recover(ctx, false); err == nil {
				t.Fatal("unconfirmed GC recovery succeeded")
			}
			if _, err := controller.Recover(ctx, true); err != nil {
				t.Fatal(err)
			}
			if state := gate.State(); state.State != "open" || state.Epoch <= oldEpoch {
				t.Fatalf("recovered GC epoch = %#v", state)
			}
			if _, err := f.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphBlob, blob.digest, blob.size, 0, gate.State().Epoch); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("old observation reused after GC = %v", err)
			}
			if err := f.processor.verifyOrReuseLocalBlob(ctx, task, blob, strings.Repeat("c", 32), path); err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 2 {
				t.Fatalf("Distribution body reads across GC = %d, want 2", reads.Load())
			}
		})
	}
}

func TestStagedFinalChecksAndPublicationUseSingleWriteAdmission(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.service.gate.mu.Lock()
		active := f.service.gate.active
		f.service.gate.mu.Unlock()
		if active > 1 || r.Method != http.MethodHead && active != 1 {
			t.Errorf("%s %s has %d write admissions; body checks and writes require one, presence probes allow zero", r.Method, r.URL.Path, active)
		}
		f.serveHTTP(w, r)
	}))
	f.service.registry, f.processor.registry = registry, registry
	blob := f.cacheObject([]byte("single admission lifecycle"), "application/octet-stream")
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &blob, Layers: []artifact.Descriptor{}})
	root := f.root("single-admission", descriptor)
	var yielded *tasks.Yield
	if err := f.step(root.ID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps {
		t.Fatalf("root discovery = %v", err)
	}
	f.transfer(blob)
	if err := f.step(root.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.published("single-admission"); !ok {
		t.Fatal("root tag was not published")
	}
	f.service.gate.mu.Lock()
	active := f.service.gate.active
	f.service.gate.mu.Unlock()
	if active != 0 {
		t.Fatalf("completed publication retained %d admissions", active)
	}
}

func TestGCStartRetryAndRecoveryPreserveStartupBarrier(t *testing.T) {
	for _, operation := range []string{"start", "retry", "recover"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			f.service.ctx, f.service.cfg.GCMode = ctx, "local"
			f.service.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
			f.service.registry = registry
			controller := &gcController{service: f.service}
			if operation != "start" {
				if err := f.service.gate.BeginDrain(ctx); err != nil {
					t.Fatal(err)
				}
				if err := f.service.gate.SetGCState(ctx, "failed_unknown", "local:test", "executor_unknown"); err != nil {
					t.Fatal(err)
				}
				controller.run = gcRun{SchemaVersion: 1, ID: strings.Repeat("d", 32), Mode: "local", State: "failed_unknown"}
			}
			if _, err := f.service.gate.BeginStartupRecovery(ctx); err != nil {
				t.Fatal(err)
			}
			var err error
			switch operation {
			case "start":
				_, err = controller.Trigger(false)
			case "retry":
				_, err = controller.Retry(ctx, true)
			case "recover":
				_, err = controller.Recover(ctx, true)
			}
			wantError := ErrRecoveryBarrier
			if operation == "recover" {
				wantError = ErrGCProtected
			}
			if !errors.Is(err, wantError) {
				t.Fatalf("GC %s during startup recovery = %v", operation, err)
			}
			if !f.service.gate.State().RecoveryBarrier {
				t.Fatalf("GC %s cleared startup barrier", operation)
			}
			if _, err := f.service.gate.Enter(ctx); !errors.Is(err, ErrRecoveryBarrier) && !errors.Is(err, ErrWritesClosed) {
				t.Fatalf("write after GC %s during startup recovery = %v", operation, err)
			}
			if controller.cancel != nil {
				t.Fatal("startup recovery launched a GC executor")
			}
		})
	}
}
