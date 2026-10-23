package download

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"registry-hub/internal/storage"
)

func TestQuotaConflictRetriesAreBoundedAndRespectCapacity(t *testing.T) {
	t.Run("maximum attempts", func(t *testing.T) {
		ctx, backing := testDownloadStore(t)
		store := &downloadFaultStore{Store: backing, quotaConflicts: 32}
		manager := New(store, 1024)
		waits := 0
		manager.quotaWait = func(context.Context, int) error {
			waits++
			return nil
		}
		err := manager.reserve(ctx, Request{Identity: "registry/conflicts", Digest: "sha256:conflicts"}, "session", 50)
		if !errors.Is(err, storage.ErrConditionFailed) || waits != 15 || store.putCount(quotaKey) != 16 {
			t.Fatalf("conflict result=%v waits=%d puts=%d, want 16 attempts and 15 waits", err, waits, store.putCount(quotaKey))
		}
	})
	t.Run("concurrent reservations", func(t *testing.T) {
		ctx, backing := testDownloadStore(t)
		store := &downloadFaultStore{Store: backing}
		manager := New(store, 100)
		manager.quotaWait = func(context.Context, int) error { return nil }
		const workers = 8
		results := make(chan error, workers)
		var group sync.WaitGroup
		for index := 0; index < workers; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				request := Request{Identity: fmt.Sprintf("registry/concurrent-%d", index), Digest: fmt.Sprintf("sha256:concurrent-%d", index)}
				results <- manager.reserve(ctx, request, fmt.Sprintf("session-%d", index), 25)
			}(index)
		}
		group.Wait()
		close(results)
		succeeded, exhausted := 0, 0
		for err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrQuotaExceeded):
				exhausted++
			default:
				t.Fatalf("concurrent reserve returned unexpected error: %v", err)
			}
		}
		used, sessions, err := manager.Usage(ctx)
		if err != nil || succeeded != 4 || exhausted != 4 || used != 100 || sessions != 4 {
			t.Fatalf("concurrent reservations succeeded=%d exhausted=%d usage=(%d,%d,%v)", succeeded, exhausted, used, sessions, err)
		}
	})
}

func TestQuotaReservationNoOpAndConflictBackoff(t *testing.T) {
	ctx, backing := testDownloadStore(t)
	store := &downloadFaultStore{Store: backing}
	manager := New(store, 1024)
	var waits []int
	manager.quotaWait = func(_ context.Context, conflict int) error {
		waits = append(waits, conflict)
		return nil
	}
	request := Request{Identity: "registry/quota", Digest: "sha256:quota"}
	if err := manager.reserve(ctx, request, "session-a", 100); err != nil {
		t.Fatalf("initial reserve: %v", err)
	}
	before := store.putCount(quotaKey)
	if err := manager.reserve(ctx, request, "session-a", 80); err != nil {
		t.Fatalf("unchanged reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before {
		t.Fatalf("unchanged reservation PUT count = %d, want %d", got, before)
	}
	if err := manager.reserve(ctx, request, "session-a", 120); err != nil {
		t.Fatalf("growing reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before+1 {
		t.Fatalf("growing reservation PUT count = %d, want %d", got, before+1)
	}
	if err := manager.reserve(ctx, request, "session-b", 120); err != nil {
		t.Fatalf("same-size new-session reserve: %v", err)
	}
	if got := store.putCount(quotaKey); got != before+2 {
		t.Fatalf("new-session reservation PUT count = %d, want %d", got, before+2)
	}

	store.quotaConflicts = 2
	conflictRequest := Request{Identity: "registry/conflict", Digest: "sha256:conflict"}
	if err := manager.reserve(ctx, conflictRequest, "session-c", 50); err != nil {
		t.Fatalf("reserve after CAS conflicts: %v", err)
	}
	if len(waits) != 2 || waits[0] != 0 || waits[1] != 1 {
		t.Fatalf("quota conflict waits = %v, want [0 1]", waits)
	}
	if quotaBackoffLimit(0) != 10*time.Millisecond || quotaBackoffLimit(5) != 250*time.Millisecond || quotaBackoffLimit(20) != 250*time.Millisecond {
		t.Fatal("quota conflict backoff limits are not capped as configured")
	}
}
