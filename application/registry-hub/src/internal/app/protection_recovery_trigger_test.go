package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"testing"
)

func TestRecoveryTriggerSurvivesClaimsAndRestart(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	ctx := withRecoveryTrigger(context.Background(), "terminal_cleanup", "demand_delete_unknown", "task", "work", "attempt", storage.ErrTimeout)
	if err := f.service.gate.SetRecoveryBarrier(ctx, "demand_delete_unknown"); err != nil {
		t.Fatal(err)
	}
	original := *f.service.gate.State().RecoveryTrigger
	for fence := uint64(1); fence <= 3; fence++ {
		gate, err := newWriteGate(context.Background(), f.service.store)
		if err != nil {
			t.Fatal(err)
		}
		state, err := gate.claimProtectionRecovery(recoveryClaimContext(), fence)
		if err != nil || state.RecoveryTrigger == nil || *state.RecoveryTrigger != original || state.RecoveryReason != "startup_reconcile" {
			t.Fatalf("takeover overwrote trigger or recovery mode: %+v %v", state, err)
		}
	}
}

func TestRecoveryClaimPreservesLocalTriggerAfterFailedBarrierWrite(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	failed := false
	faults.putHook = func(key string, _ []byte) error {
		if key == "system/gc/state.json" && !failed {
			failed = true
			return storage.ErrUnavailable
		}
		return nil
	}
	ctx := withRecoveryTrigger(context.Background(), "terminal_cleanup", "runtime_write_unknown", "task", "work", "attempt", storage.ErrUnknownCommit)
	if err := f.service.gate.SetRecoveryBarrier(ctx, "runtime_write_unknown"); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	state, err := f.service.gate.claimProtectionRecovery(recoveryClaimContext(), 1)
	if err != nil || state.RecoveryTrigger == nil || state.RecoveryTrigger.Kind != "terminal_cleanup" || state.RecoveryTrigger.WorkID != "work" {
		t.Fatalf("failed persistence lost local trigger: %+v %v", state, err)
	}
}

func recoveryClaimContext() context.Context {
	return context.WithValue(context.Background(), recoveryWriteGuardKey{}, recoveryWriteGuard(func(context.Context) error { return nil }))
}

func TestRecoveryClaimDoesNotInventStartupTriggerForLegacyBarrier(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	f.service.gate.mu.Lock()
	f.service.gate.state.RecoveryBarrier = true
	if err := f.service.gate.persistLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.service.gate.mu.Unlock()
	state, err := f.service.gate.claimProtectionRecovery(context.Background(), 1)
	if err != nil || state.RecoveryTrigger == nil || state.RecoveryTrigger.Kind != "unknown" {
		t.Fatalf("legacy barrier mislabeled as startup: %+v %v", state, err)
	}
}

func TestRecoveryTriggerRecordsFreshStartupAndPreservesRuntimeBarrier(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	if _, err := f.service.gate.BeginStartupRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := f.service.gate.claimProtectionRecovery(recoveryClaimContext(), 1)
	if err != nil || state.RecoveryTrigger == nil || state.RecoveryTrigger.Kind != "startup" {
		t.Fatalf("startup cause missing: %+v %v", state, err)
	}
	before := *state.RecoveryTrigger
	if _, err := f.service.gate.BeginStartupRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := f.service.gate.State().RecoveryTrigger; after == nil || *after != before {
		t.Fatalf("existing barrier trigger was replaced: %+v", after)
	}
}
