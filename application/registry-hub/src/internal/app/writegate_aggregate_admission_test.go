package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"registry-hub/internal/storage"
)

func TestConcurrentScopeAdmissionsCannotExceedAggregateByteQuota(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1000)
	start := make(chan struct{})
	results := make(chan error, 32)
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		id := mustGateTestID(t)
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- f.service.gate.AdmitProtection(ctx, id, "concurrent-scope-"+id, id, 100, 32, 1000)
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, ErrProtectionCapacity) {
			t.Fatalf("concurrent admission = %v", err)
		}
	}
	gate, err := newWriteGate(ctx, f.service.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.ValidateProtectionState(ctx); err != nil {
		t.Fatal(err)
	}
	state := gate.State()
	if admitted != 10 || state.ProtectionSessionCount != 10 || state.ReservedDistributionBytes != 1000 {
		t.Fatalf("admitted=%d recovered ledger=%#v", admitted, state)
	}
}

func TestIndependentCoordinatorsCannotOverreserveAggregateByteQuota(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 1000)
	second, err := newWriteGate(ctx, f.service.store)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, gate := range []*writeGate{f.service.gate, second} {
		id := mustGateTestID(t)
		go func() {
			<-start
			results <- gate.AdmitProtection(ctx, id, "cas-scope-"+id, id, 700, 10, 1000)
		}()
	}
	close(start)
	admitted := 0
	for range 2 {
		if err := <-results; err == nil {
			admitted++
		} else if !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, ErrProtectionCapacity) {
			t.Fatalf("concurrent coordinator admission = %v", err)
		}
	}
	gate, err := newWriteGate(ctx, f.service.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.ValidateProtectionState(ctx); err != nil {
		t.Fatal(err)
	}
	if state := gate.State(); admitted != 1 || state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 700 || state.RecoveryBarrier {
		t.Fatalf("CAS admitted=%d ledger=%#v", admitted, state)
	}
}

func TestMultiSourceProtectionMergePreservesOwnersAndReplacesQuota(t *testing.T) {
	ctx := context.Background()
	f := newStagedAcceptanceFixture(t, 300)
	gate := f.service.gate
	first, second, merged := mustGateTestID(t), mustGateTestID(t), mustGateTestID(t)
	firstOwner, secondOwner := mustGateTestID(t), mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, first, "direct-first", firstOwner, 100, 2, 300); err != nil {
		t.Fatal(err)
	}
	if err := gate.AddProtectionOwner(ctx, first, secondOwner); err != nil {
		t.Fatal(err)
	}
	if err := gate.AdmitProtection(ctx, second, "direct-second", secondOwner, 100, 2, 300); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReplaceProtectionSet(ctx, []string{first, second}, merged, "merged-platform", firstOwner, 300, 2, 300); err != nil {
		t.Fatalf("merge while only replacement credit fits: %v", err)
	}
	gate, err := newWriteGate(ctx, f.service.store)
	if err != nil {
		t.Fatal(err)
	}
	if err := gate.ValidateProtectionState(ctx); err != nil {
		t.Fatal(err)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, merged)
	if err != nil || len(owners) != 2 {
		t.Fatalf("merged owners=%v error=%v", owners, err)
	}
	state := gate.State()
	if state.ProtectionSessionCount != 1 || state.NewProtectionSessionCount != 1 || state.ReservedDistributionBytes != 300 {
		t.Fatalf("merged quota ledger = %#v", state)
	}
	for _, source := range []string{first, second} {
		session, err := gate.readProtectionSession(ctx, source)
		if err != nil || session.State != "transferred" || session.ReservationID != "" || session.TerminalReason != "scope_merged" {
			t.Fatalf("source session = %#v error=%v", session, err)
		}
	}
	if remaining, err := gate.RemoveProtectionOwner(ctx, merged, firstOwner); err != nil || remaining != 1 {
		t.Fatalf("shared cancellation remaining=%d error=%v", remaining, err)
	}
	if stopped, err := gate.RequestProtectionStop(ctx, merged, "cancelled"); err != nil || stopped {
		t.Fatalf("shared merged protection stopped=%t error=%v", stopped, err)
	}
	if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC after one merged owner cancels = %v", err)
	}
}
