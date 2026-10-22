package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func TestGCAdmissionRejectsProtectedTaskGapsWaitsAndRetriesAfterRestart(t *testing.T) {
	for _, mode := range []string{"local", "kubernetes"} {
		for _, state := range []tasks.State{tasks.Pending, tasks.WaitingDeps, tasks.RetryWait} {
			t.Run(fmt.Sprintf("%s/%s", mode, state), func(t *testing.T) {
				ctx := context.Background()
				f := newStagedAcceptanceFixture(t, 1<<20)
				descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
				root := f.root("protected-gap", descriptor)
				root.Status = state
				if state == tasks.WaitingDeps {
					root.WaitReason = "dependencies_unavailable"
				}
				currentLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+root.ID, "protected-gap", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.tasks.SaveProgress(ctx, &root, currentLease); err != nil {
					t.Fatal(err)
				}
				if err := f.service.tasks.Leases().Release(ctx, currentLease); err != nil {
					t.Fatal(err)
				}
				sessionID := mustGateTestID(t)
				if err := f.service.gate.AdmitProtection(ctx, sessionID, "unanchored-platform", root.ID, 100, 10, 1024); err != nil {
					t.Fatal(err)
				}
				if err := f.service.gate.MarkProtectionContentObserved(ctx, sessionID); err != nil {
					t.Fatal(err)
				}
				f.service.ctx = ctx
				f.service.cfg.GCMode = mode
				f.service.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
				for _, restart := range []bool{false, true} {
					if restart {
						gate, err := newWriteGate(ctx, f.service.store)
						if err != nil {
							t.Fatal(err)
						}
						f.service.gate = gate
					}
					controller, err := newGCController(f.service)
					if err != nil {
						t.Fatal(err)
					}
					_, err = controller.Trigger(false)
					controller.Close()
					if !errors.Is(err, ErrGCProtected) && !errors.Is(err, ErrRecoveryBarrier) {
						t.Fatalf("GC admission restart=%t = %v, want retained blocker", restart, err)
					}
					if controller.Snapshot().ID != "" || f.service.gate.State().State != "open" {
						t.Fatal("blocked GC created a run or entered draining")
					}
				}
				if _, err := f.service.store.Head(ctx, "system/gc/last-run.json"); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("blocked executor record = %v, want absent", err)
				}
				if f.requests != 0 {
					t.Fatalf("blocked GC contacted Distribution %d times", f.requests)
				}
			})
		}
	}
}

func TestGCAdmittedFirstPreventsRootAndBlobTransferOrObservation(t *testing.T) {
	for _, kind := range []tasks.Kind{tasks.RootKind, tasks.BlobKind} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			f := newStagedAcceptanceFixture(t, 1<<20)
			blob := f.cacheObject([]byte("GC admitted before this transfer"), "application/octet-stream")
			var taskID string
			if kind == tasks.RootKind {
				descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType,
					Config: &blob, Layers: []artifact.Descriptor{}})
				taskID = f.root("gc-first", descriptor).ID
			} else {
				intent, err := f.service.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
					Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest,
				})
				if err != nil {
					t.Fatal(err)
				}
				cached, err := f.service.cache.GetAnyRetained(ctx, "ghcr.io", "team/image", blob.Digest)
				if err != nil {
					t.Fatal(err)
				}
				err = f.service.activateBlobIntent(ctx, intent, cached)
				_ = cached.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				taskID = intent.ID
			}
			if err := f.service.gate.BeginDrain(ctx); err != nil {
				t.Fatal(err)
			}
			var yielded *tasks.Yield
			if err := f.step(taskID); !errors.As(err, &yielded) || yielded.Status != tasks.WaitingDeps || yielded.Reason != "gc_admission" {
				t.Fatalf("processing while GC admitted = %v, want gc_admission wait", err)
			}
			if f.requests != 0 {
				t.Fatalf("GC-blocked worker made %d Distribution calls", f.requests)
			}
			if state := f.service.gate.State(); state.State != "draining" || state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
				t.Fatalf("GC-blocked worker changed admission: %#v", state)
			}
			if removed, err := f.service.cache.Cleanup(ctx, time.Now().Add(time.Minute), 100); err != nil || removed != 0 {
				t.Fatalf("GC-blocked cleanup = %d, %v", removed, err)
			}
		})
	}
}
