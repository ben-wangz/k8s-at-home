package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
	"time"
)

func TestProtectionRecoveryResumesExistingReferencesAcrossBudgetsAndRestart(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	for root := 0; root < 8; root++ {
		task := f.root(fmt.Sprintf("resumable-%d", root), descriptor)
		for node := 0; node < 40; node++ {
			digest := artifact.CalculateSHA256([]byte(fmt.Sprintf("blob-%d", node)))
			task.Graph[digest] = tasks.GraphEntry{Digest: digest, Size: 1, MediaType: "application/octet-stream"}
		}
		saveLegacyAcceptanceTask(t, f, &task)
		for _, digest := range taskRecoveryDigests(task) {
			if err := f.service.records.EnsureCacheProtectionReference(context.Background(), task.ID, task.Authority, task.Repository, digest); err != nil {
				t.Fatal(err)
			}
		}
	}
	faults.reads = map[string]int{}
	faults.getHook = func(key string) error {
		if strings.HasPrefix(key, "solidification/cache-protection/") {
			time.Sleep(2 * time.Millisecond)
		}
		return nil
	}
	var generation string
	var processed uint64
	quantum := time.Second
	started := time.Now()
	rounds := 0
	for ; rounds < 100; rounds++ {
		parent := context.WithValue(context.Background(), backgroundParentKey{}, context.Background())
		ctx, cancel := context.WithTimeout(parent, quantum)
		err := f.service.recoverProtectionQuantum(ctx)
		cancel()
		if err != nil && !errors.Is(err, errProtectionRecoveryYield) {
			t.Fatalf("round %d: %v", rounds, err)
		}
		checkpoint := recoveryCheckpointForTest(t, f.service)
		if generation == "" {
			generation = checkpoint.Generation
		}
		if checkpoint.Generation != generation || checkpoint.Processed < processed {
			t.Fatalf("lost durable progress: %+v", checkpoint)
		}
		processed = checkpoint.Processed
		if err == nil {
			break
		}
		if !f.service.gate.State().RecoveryBarrier {
			t.Fatal("yield released the barrier")
		}
		gate, err := newWriteGate(context.Background(), f.service.store)
		if err != nil {
			t.Fatal(err)
		}
		gate.recovery = f.service.gate.recovery
		f.service.gate, f.processor.gate = gate, gate
		f.service.workerStates = nil
		f.service.instanceID = requestID()
	}
	if rounds == 0 || rounds >= 100 || time.Since(started) <= quantum || f.service.gate.State().RecoveryBarrier {
		t.Fatalf("recovery did not converge across budgets: rounds=%d", rounds)
	}
	reads := 0
	for key, count := range faults.reads {
		if strings.HasPrefix(key, "solidification/cache-protection/") {
			reads += count
			if count > 2 {
				t.Fatalf("reference repeatedly rescanned %d times: %s", count, key)
			}
		}
	}
	if reads > 8*41*2+rounds {
		t.Fatalf("completed references were rescanned: %d reads, %d rounds", reads, rounds)
	}
	if recoveryCheckpointForTest(t, f.service).Yields == 0 {
		t.Fatal("scale fixture never yielded")
	}
}

func TestProtectionRecoveryResumesBetweenReferenceHalves(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	descriptor := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	f.root("reference-half", descriptor)
	faults.reads = map[string]int{}
	failed := false
	faults.getHook = func(key string) error {
		if !failed && strings.HasPrefix(key, "solidification/cache-protection/digests/") {
			failed = true
			return storage.ErrTimeout
		}
		return nil
	}
	if err := f.service.recoverSolidificationProtection(context.Background()); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("expected reference fault: %v", err)
	}
	c := recoveryCheckpointForTest(t, f.service)
	if c.Phase != "cache_protection_index" || c.Half != 1 || c.LastError == nil {
		t.Fatalf("reference half was lost: %+v", c)
	}
	if err := f.service.recoverSolidificationProtection(context.Background()); err != nil {
		t.Fatal(err)
	}
	for key, count := range faults.reads {
		if strings.HasPrefix(key, "solidification/cache-protection/tasks/") && count != 1 {
			t.Fatalf("replayed completed half: %s=%d", key, count)
		}
	}
	if c := recoveryCheckpointForTest(t, f.service); c.LastError == nil || c.RecoveredAt.Before(c.LastError.OccurredAt) {
		t.Fatal("recovery lost failure history")
	}
}

func TestProtectionRecoveryReconcilesFailedBarrierWriteWithoutOpeningUnknownGC(t *testing.T) {
	for _, durableState := range []string{"open", "failed_unknown"} {
		t.Run(durableState, func(t *testing.T) {
			f, faults := newRecoveryFixture(t)
			if durableState != "open" {
				f.service.gate.state.State = durableState
				if err := f.service.gate.persist(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			failed := false
			faults.putHook = func(key string, _ []byte) error {
				if key == "system/gc/state.json" && !failed {
					failed = true
					return storage.ErrUnknownCommit
				}
				return nil
			}
			if err := f.service.gate.SetRecoveryBarrier(context.Background(), "demand_index_delete_unknown"); !errors.Is(err, storage.ErrUnknownCommit) {
				t.Fatal(err)
			}
			if f.service.gate.State().State != "failed_unknown" {
				t.Fatal("failed barrier write did not close local gate")
			}
			if err := f.service.recoverSolidificationProtection(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := f.service.gate.State()
			if state.RecoveryBarrier || state.State != durableState {
				t.Fatalf("incorrect gate reconciliation: %+v", state)
			}
		})
	}
}

func TestProtectionRecoveryRejectsStaleOwnerMutationsAndCompletion(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	r, staleCtx, release, err := f.service.openProtectionRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	newOwner, _, nextRelease, err := f.service.openProtectionRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer nextRelease()
	staleCtx = context.WithValue(context.Background(), recoveryWriteGuardKey{}, recoveryWriteGuard(r.guard))
	if newOwner.checkpoint.Generation != r.checkpoint.Generation || newOwner.checkpoint.Fence <= r.checkpoint.Fence {
		t.Fatal("takeover discarded generation or fence")
	}
	if err := r.save(context.Background()); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale checkpoint was accepted: %v", err)
	}
	if _, err := f.service.store.Put(staleCtx, "solidification/stale.json", strings.NewReader("{}"), 2, storage.PutOptions{IfAbsent: true}); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale source mutation was accepted: %v", err)
	}
	if err := f.service.gate.completeProtectionRecovery(staleCtx, r.checkpoint.Generation, r.checkpoint.Fence, r.checkpoint.ValidationLedger); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale completion was accepted: %v", err)
	}
	if !f.service.gate.State().RecoveryBarrier {
		t.Fatal("stale owner released the barrier")
	}
}
