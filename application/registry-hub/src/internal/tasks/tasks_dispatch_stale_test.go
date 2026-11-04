package tasks

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"testing"
	"time"
)

func TestProcessRevalidatesStaleDispatchUnderLease(t *testing.T) {
	for _, state := range []State{Solidified, Failed, Cancelled, WaitingDeps, WaitingQuota, VerifiedLocal, RepairPending, RetryWait} {
		t.Run(string(state), func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			processor := &retryRecoveryProcessor{seen: make(chan Task, 1)}
			manager, err := NewManager(store, 1, 10, processor)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			stale, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
			if err != nil {
				t.Fatal(err)
			}
			stale.Phase = "solidification"
			if err := manager.save(ctx, &stale); err != nil {
				t.Fatal(err)
			}
			current := stale
			current.Status = state
			if state == RetryWait {
				current.NextAttemptAt = time.Now().UTC().Add(time.Minute)
			}
			if state == Solidified || state == Failed || state == Cancelled {
				current.TerminalCleanupID = current.ID
				current.ScopeID = current.ID
				current.SessionIDs = []string{current.ID}
				current.GCProtected = true
			}
			if err := manager.save(ctx, &current); err != nil {
				t.Fatal(err)
			}
			manager.process(ctx, 0, stale)
			select {
			case <-processor.seen:
				t.Fatal("stale dispatch executed a task that is no longer runnable")
			default:
			}
			actual := mustGetTask(t, manager, current.ID)
			if actual.version != current.version || actual.Status != current.Status || actual.TerminalCleanupID != current.TerminalCleanupID || actual.GCProtected != current.GCProtected {
				t.Fatalf("stale dispatch changed task state or pending cleanup: %#v", actual)
			}
		})
	}
}
