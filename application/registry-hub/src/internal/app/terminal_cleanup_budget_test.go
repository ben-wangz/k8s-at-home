package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestTerminalLeaseValidationReadDeadlineRemainsBudgetYield(t *testing.T) {
	f, faults, root := terminalFixture(t, 0)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	held, err := manager.Acquire(ctx, "tasks/"+root.ID, "read-budget", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Release(ctx, held)
	faults.getHook = func(key string) error {
		if strings.HasPrefix(key, "leases/tasks/") {
			time.Sleep(80 * time.Millisecond)
		}
		return nil
	}
	scope := &terminalWriteScope{manager: manager, lease: held}
	bounded, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err = scope.guard(bounded)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, storage.ErrLeaseLost) || f.service.gate.State().RecoveryBarrier {
		t.Fatalf("lease read deadline was misclassified as lost authority: %v", err)
	}
}
