package app

import (
	"context"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestTerminalExpiryCleanupResumesAcrossBudgetsAndRestartWithoutBarrier(t *testing.T) {
	f, faults, root := terminalFixture(t, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	expired, err := f.service.tasks.CancelIfExpired(ctx, root.ID, time.Now().Add(48*time.Hour))
	cancel()
	if err != nil || !expired {
		t.Fatalf("expiry did not hand off bounded cleanup: %t %v", expired, err)
	}
	root, err = f.service.tasks.Get(context.Background(), root.ID)
	if err != nil || root.TerminalCleanupID == "" || root.TerminalCleanupComplete || !root.GCProtected {
		t.Fatalf("enqueue released protection: %+v %v", root, err)
	}
	faults.getHook = func(key string) error {
		if strings.HasPrefix(key, "solidification/blobs/root-owners/") {
			time.Sleep(4 * time.Millisecond)
		}
		return nil
	}
	var processed uint64
	completed := false
	started := time.Now()
	for round := 0; time.Since(started) < 5*time.Minute; round++ {
		p, done, err := terminalQuantum(t, f.service, root, time.Second)
		if err != nil || p.Processed < processed {
			t.Fatalf("round %d lost progress: %+v %v", round, p, err)
		}
		processed = p.Processed
		gate := f.service.gate.State()
		if gate.RecoveryBarrier || gate.State != "open" || f.service.gate.recovery.blocked.Load() {
			t.Fatalf("normal cleanup yield closed writes: %+v", gate)
		}
		if done {
			completed = true
			break
		}
		if round == 1 {
			restarted, err := newWriteGate(context.Background(), f.service.store)
			if err != nil {
				t.Fatal(err)
			}
			restarted.recovery = f.service.gate.recovery
			f.service.gate, f.processor.gate = restarted, restarted
			f.service.instanceID = requestID()
		}
	}
	current, err := f.service.tasks.Get(context.Background(), root.ID)
	if !completed || err != nil || !current.TerminalCleanupComplete || current.GCProtected || processed < 32 || time.Since(started) <= 100*time.Millisecond {
		t.Fatalf("cleanup did not converge across quanta: complete=%t processed=%d task=%+v error=%v", completed, processed, current, err)
	}
	job := terminalJob(t, f.service, root)
	if job.RetryCount != 0 || job.Fence < 2 {
		t.Fatalf("normal yields consumed storage retries: %+v", job)
	}
}

func TestTerminalReadDeadlineYieldsWithoutMutationOrRecovery(t *testing.T) {
	f, faults, root := terminalFixture(t, 1)
	root, err := f.service.tasks.Cancel(context.Background(), root.ID)
	if err != nil {
		t.Fatal(err)
	}
	faults.getHook = func(key string) error {
		if strings.HasPrefix(key, "solidification/blobs/root-owners/") {
			time.Sleep(80 * time.Millisecond)
		}
		return nil
	}
	p, done, err := terminalQuantum(t, f.service, root, 50*time.Millisecond)
	if err != nil || done || p.Pending != nil || p.YieldReason != "execution_budget" || f.service.gate.State().RecoveryBarrier {
		t.Fatalf("read deadline escalated to recovery: %+v done=%t error=%v gate=%+v", p, done, err, f.service.gate.State())
	}
	faults.getHook = nil
	drainBackgroundWork(t, f.service, "terminal_cleanup")
}

func TestTerminalReconciliationRepairsMissedHandoff(t *testing.T) {
	f, faults, root := terminalFixture(t, 1)
	faults.putHook = func(key string, _ []byte) error {
		if strings.HasPrefix(key, "background/v1/jobs/terminal_cleanup/") {
			return storage.ErrPermissionDenied
		}
		return nil
	}
	current, err := f.service.tasks.Cancel(context.Background(), root.ID)
	if err != nil || current.TerminalCleanupID == "" || current.TerminalCleanupComplete {
		t.Fatalf("terminal outbox not retained: %+v %v", current, err)
	}
	if f.service.gate.State().RecoveryBarrier {
		t.Fatal("enqueue failure closed global writes")
	}
	faults.putHook = nil
	if err := f.service.reconcileTerminalTaskCleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	drainBackgroundWork(t, f.service, "terminal_cleanup")
	current, err = f.service.tasks.Get(context.Background(), root.ID)
	if err != nil || !current.TerminalCleanupComplete || current.GCProtected {
		t.Fatalf("missed handoff not repaired: %+v %v", current, err)
	}
	if gate := f.service.gate.State(); gate.ReservedDistributionBytes != 0 || gate.ProtectionSessionCount != 0 || gate.RecoveryBarrier {
		t.Fatalf("repaired cleanup left protection accounting inconsistent: %+v", gate)
	}
}
