package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/registryapi"
	"strings"
	"sync/atomic"
	"testing"
)

func recoveryAbsentWrite(kind string) protectionRemoteWrite {
	digest := artifact.CalculateSHA256([]byte("interrupted write"))
	write := protectionRemoteWrite{Kind: kind, Digest: digest, Size: 17, MediaType: ociManifestMediaType}
	write.Path = "/v2/recovery/manifests/" + digest
	if kind == "blob" {
		write.Path = "/v2/recovery/blobs/" + digest
	}
	if kind == "root_tag" {
		write.Path = "/v2/recovery/manifests/latest"
	}
	if kind == "referrer" {
		write.Subject = artifact.CalculateSHA256([]byte("subject"))
		write.Path = "/v2/recovery/referrers/" + write.Subject
		write.FallbackPath = "/v2/recovery/manifests/fallback"
	}
	return write
}

func admitInterruptedRecoveryWrite(t *testing.T, f *stagedAcceptanceFixture, write protectionRemoteWrite) string {
	t.Helper()
	ctx := context.Background()
	id := mustGateTestID(t)
	if err := f.service.gate.AdmitProtection(ctx, id, "recovery-scope", "", write.Size, 4, 1024); err != nil {
		t.Fatal(err)
	}
	if err := f.service.gate.MarkProtectionWriteStarted(ctx, id, write); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestProtectionRecoveryResumesAbsentWritesWithoutReleasingProtection(t *testing.T) {
	for _, kind := range []string{"blob", "manifest", "root_tag", "referrer"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := newRecoveryFixture(t)
			write := recoveryAbsentWrite(kind)
			id := admitInterruptedRecoveryWrite(t, f, write)
			var requests atomic.Int32
			f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				code := "MANIFEST_UNKNOWN"
				if strings.Contains(r.URL.Path, "/blobs/") {
					code = "BLOB_UNKNOWN"
				}
				registryapi.WriteError(w, http.StatusNotFound, code, "not committed")
			}))
			ctx := context.Background()
			if err := f.service.recoverSolidificationProtection(ctx); err != nil {
				t.Fatal(err)
			}
			session, active, err := f.service.gate.protectionSession(ctx, id)
			if err != nil || !active || session.State != "protected" || session.UncertainRemoteResult || session.RemoteWrite != nil || session.PendingOperations != 0 || !session.MayContainUnanchored {
				t.Fatalf("absent write was not reconciled conservatively: %+v active=%t err=%v", session, active, err)
			}
			state := f.service.gate.State()
			if state.RecoveryBarrier || state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != write.Size {
				t.Fatalf("lost recovery or reservation state: %+v", state)
			}
			if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC admitted an unanchored session: %v", err)
			}
			finish, err := f.service.gate.BeginProtectionOperation(ctx, id)
			if err != nil {
				t.Fatal("retry remained blocked:", err)
			}
			if err := f.service.gate.MarkProtectionWriteStarted(ctx, id, write); err != nil {
				t.Fatal(err)
			}
			if err := finish(ctx); err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if kind == "referrer" {
				want = 2
			}
			if requests.Load() != want {
				t.Fatalf("absence caused repeated remote reads: got %d want %d", requests.Load(), want)
			}
		})
	}
}

func TestRuntimeProtectionReconciliationResolvesWriteWithoutRecoveryBarrier(t *testing.T) {
	t.Run("verified blob", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		write := recoveryAbsentWrite("blob")
		body := []byte("interrupted write")
		if artifact.CalculateSHA256(body) != write.Digest || int64(len(body)) != write.Size {
			t.Fatalf("test payload does not match write descriptor: %s/%d", artifact.CalculateSHA256(body), len(body))
		}
		f.objects[write.Digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
		id := admitInterruptedRecoveryWrite(t, f, write)
		if err := f.service.reconcileRuntimeProtectionWrite(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		session, active, err := f.service.gate.protectionSession(context.Background(), id)
		if err != nil || !active || session.UncertainRemoteResult || session.RemoteWrite != nil || !session.MayContainUnanchored {
			t.Fatalf("verified blob reconciliation lost the protected state: %+v active=%t err=%v", session, active, err)
		}
		if f.service.gate.State().RecoveryBarrier {
			t.Fatal("runtime reconciliation raised the global recovery barrier")
		}
	})

	t.Run("confirmed miss", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		write := recoveryAbsentWrite("blob")
		id := admitInterruptedRecoveryWrite(t, f, write)
		if err := f.service.reconcileRuntimeProtectionWrite(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		session, active, err := f.service.gate.protectionSession(context.Background(), id)
		if err != nil || !active || session.UncertainRemoteResult || session.RemoteWrite != nil || !session.MayContainUnanchored {
			t.Fatalf("runtime miss reconciliation lost the protected state: %+v active=%t err=%v", session, active, err)
		}
		if f.service.gate.State().RecoveryBarrier {
			t.Fatal("runtime reconciliation raised the global recovery barrier")
		}
	})

	t.Run("untrusted registry result keeps protection", func(t *testing.T) {
		f := newStagedAcceptanceFixture(t, 1<<20)
		write := recoveryAbsentWrite("blob")
		id := admitInterruptedRecoveryWrite(t, f, write)
		f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		if err := f.service.reconcileRuntimeProtectionWrite(context.Background(), id); err == nil {
			t.Fatal("unavailable registry result was treated as resolved")
		}
		session, active, err := f.service.gate.protectionSession(context.Background(), id)
		if err != nil || !active || !session.UncertainRemoteResult || session.RemoteWrite == nil || session.RemoteRecovery == nil || session.RemoteRecovery.Attempts != 1 || session.RemoteRecovery.ErrorClass != "distribution_reconcile" {
			t.Fatalf("untrusted registry result released protection or missed retry state: %+v active=%t err=%v", session, active, err)
		}
		if f.service.gate.State().RecoveryBarrier {
			t.Fatal("runtime reconciliation raised the global recovery barrier")
		}
	})
}

func TestProtectionRecoveryCheckpointsAbsentWriteAcrossOwners(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	write := recoveryAbsentWrite("blob")
	id := admitInterruptedRecoveryWrite(t, f, write)
	var reads atomic.Int32
	f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	ctx := context.Background()
	r, work, release, err := f.service.openProtectionRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r.checkpoint.Phase, r.checkpoint.SessionIDs = "remote_operations", []string{id}
	for step := 0; step < 2; step++ {
		if err := r.remoteStep(work); err != nil {
			t.Fatal(err)
		}
	}
	if !r.checkpoint.Remote.Absent || r.checkpoint.Remote.Verified {
		t.Fatal("confirmed absence was confused with payload verification")
	}
	if err := r.save(work); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	r, work, release, err = f.service.openProtectionRecovery(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for step := 0; step < 2; step++ {
		if err := r.remoteStep(work); err != nil {
			t.Fatal(err)
		}
	}
	if r.checkpoint.Index != 1 || reads.Load() != 1 || !f.service.gate.State().RecoveryBarrier {
		t.Fatalf("absence was replayed or barrier lost: index=%d reads=%d", r.checkpoint.Index, reads.Load())
	}
	session, active, err := f.service.gate.protectionSession(ctx, id)
	if err != nil || !active || session.UncertainRemoteResult || !session.MayContainUnanchored {
		t.Fatalf("takeover lost protection: %+v active=%t err=%v", session, active, err)
	}
}
