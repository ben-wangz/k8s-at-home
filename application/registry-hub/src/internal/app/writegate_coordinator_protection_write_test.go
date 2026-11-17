package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"sync"
	"testing"
)

func TestProtectionWritePermitSerializesDifferentReplicaWriters(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := mustGateTestID(t)
	if err := first.AdmitProtection(ctx, sessionID, "shared-root-scope", "", 1024, 4, 4096); err != nil {
		t.Fatal(err)
	}
	second.mu.Lock()
	if err := second.reloadLocked(ctx); err != nil {
		second.mu.Unlock()
		t.Fatal(err)
	}
	second.mu.Unlock()
	type result struct {
		permitID string
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, item := range []struct {
		gate *writeGate
		id   string
	}{{first, mustGateTestID(t)}, {second, mustGateTestID(t)}} {
		group.Add(1)
		go func(gate *writeGate, permitID string) {
			defer group.Done()
			<-start
			results <- result{permitID: permitID, err: gate.AcquireProtectionWritePermit(ctx, sessionID, permitID)}
		}(item.gate, item.id)
	}
	close(start)
	group.Wait()
	close(results)
	winner := ""
	busy := 0
	for result := range results {
		if result.err == nil {
			if winner != "" {
				t.Fatal("both replicas acquired the same protection write slot")
			}
			winner = result.permitID
		} else if errors.Is(result.err, ErrProtectionBusy) {
			busy++
		} else {
			t.Fatalf("concurrent write permit acquisition failed: %v", result.err)
		}
	}
	if winner == "" || busy != 1 {
		t.Fatalf("permit winner=%q busy count=%d, want exactly one winner", winner, busy)
	}
	session, err := first.readProtectionSession(ctx, sessionID)
	if err != nil || session.RemoteWritePermit != winner || session.UncertainRemoteResult {
		t.Fatalf("serialized permit state = %+v err=%v", session, err)
	}
	if err := first.ReleaseProtectionWritePermit(ctx, sessionID, winner); err != nil {
		t.Fatal(err)
	}
	if err := second.AcquireProtectionWritePermit(ctx, sessionID, mustGateTestID(t)); err != nil {
		t.Fatalf("second replica could not acquire a released permit: %v", err)
	}
}
