package app

import (
	"bytes"
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestProtectionRecoveryClearsTaskBytesWithoutCoordinatorReservation(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	ctx := context.Background()
	intent := newReadyBlobIntent(t, ctx, f.service.tasks, f.service.records)
	if err := f.service.tasks.SetProtectedCacheBytes(ctx, intent.ID, 64); err != nil {
		t.Fatal(err)
	}
	if err := f.service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatal(err)
	}
	task, err := f.service.tasks.Get(ctx, intent.ID)
	if err != nil || task.ProtectedCacheBytes != 0 || f.service.gate.State().ProtectedCacheBytes != 0 {
		t.Fatalf("stale cache reservation survived: task=%+v err=%v", task, err)
	}
}

func TestProtectionRecoveryRejectsUnfencedSourceMutations(t *testing.T) {
	f, _ := newRecoveryFixture(t)
	r, guarded, release, err := f.service.openProtectionRecovery(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := f.service.store.Delete(context.Background(), "cache/objects/missing"); err != errRecoveryPaused {
		t.Fatalf("unfenced mutation was accepted: %v", err)
	}
	if err := r.guard(guarded); err != nil {
		t.Fatal(err)
	}
	if err := f.service.store.Delete(guarded, "cache/objects/missing"); err != nil && err != storage.ErrNotFound {
		t.Fatal(err)
	}
}

func TestProtectionRecoveryRetriesFinalCheckpointAfterGateCommit(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	failed := false
	faults.putHook = func(key string, data []byte) error {
		if key == protectionRecoveryKey && !failed && strings.Contains(string(data), `"phase":"complete"`) {
			failed = true
			return storage.ErrUnavailable
		}
		return nil
	}
	if err := f.service.recoverSolidificationProtection(context.Background()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("checkpoint fault was not reproduced: %v", err)
	}
	if f.service.gate.State().RecoveryBarrier || !f.service.gate.recovery.blocked.Load() || f.service.recoveryStarting.Load() {
		t.Fatal("fixture did not reach runtime completion uncertainty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct{})
	go func() { f.service.runProtectionRecovery(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for f.service.gate.recovery.blocked.Load() {
		select {
		case <-ctx.Done():
			t.Fatal("runtime worker did not retry the final checkpoint")
		case <-ticker.C:
		}
	}
	if c := recoveryCheckpointForTest(t, f.service); c.Phase != "complete" {
		t.Fatalf("incomplete checkpoint: %+v", c)
	}
}

func TestProtectionRecoveryQuiesceHonorsDeadline(t *testing.T) {
	controlled := &recoveryStore{}
	controlled.mutations.RLock()
	defer controlled.mutations.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := controlled.quiesce(ctx); !errors.Is(err, context.DeadlineExceeded) || !controlled.blocked.Load() {
		t.Fatalf("quiescence escaped its deadline or reopened mutations: %v", err)
	}
}

func TestProtectionRecoveryAdoptsUnknownCommittedFinalCheckpoint(t *testing.T) {
	f, faults := newRecoveryFixture(t)
	ctx := context.Background()
	failed, rejectReadback := false, false
	faults.putHook = func(key string, data []byte) error {
		if key != protectionRecoveryKey || failed || !strings.Contains(string(data), `"phase":"complete"`) {
			return nil
		}
		entry, err := faults.Store.Head(ctx, key)
		if err != nil {
			return err
		}
		if _, err := faults.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: entry.Version}); err != nil {
			return err
		}
		failed, rejectReadback = true, true
		return storage.ErrUnknownCommit
	}
	faults.getHook = func(key string) error {
		if key == protectionRecoveryKey && rejectReadback {
			rejectReadback = false
			return storage.ErrUnavailable
		}
		return nil
	}
	if err := f.service.recoverSolidificationProtection(ctx); !errors.Is(err, storage.ErrUnknownCommit) {
		t.Fatal(err)
	}
	before := recoveryCheckpointForTest(t, f.service)
	if err := f.service.recoverSolidificationProtection(ctx); err != nil {
		t.Fatal(err)
	}
	after := recoveryCheckpointForTest(t, f.service)
	if after.Generation != before.Generation || after.Processed != before.Processed+1 || f.service.gate.recovery.blocked.Load() {
		t.Fatalf("committed completion was rescanned: before=%+v after=%+v", before, after)
	}
}
