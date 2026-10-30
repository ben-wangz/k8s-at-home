package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"testing"
	"time"
)

func verifyInventoryHistoricalRefresh(t *testing.T, s *Service, leader lease.Lease, state inventoryCheckpoint) {
	t.Helper()
	ctx := context.Background()
	state.NextScanAt = time.Time{}
	started := time.Now()
	takenOver := false
	for step := 0; step < 20000; step++ {
		if step%100 == 0 {
			identity := inventoryIdentity("ghcr.io", "team/image", "changing-historical")
			if _, err := s.inventoryBinding(ctx, identity, "", "upstream", 0, time.Now().UTC(), true); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.advanceInventory(ctx, leader, &state); err != nil {
			t.Fatal(err)
		}
		if !takenOver && state.Delta != nil && state.Delta.Cursor.Offset > 0 {
			previous := leader
			if err := s.tasks.Leases().Release(ctx, leader); err != nil {
				t.Fatal(err)
			}
			var err error
			leader, err = s.tasks.Leases().Acquire(ctx, "inventory/builder", "historical-takeover", 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer s.tasks.Leases().Release(ctx, leader)
			offset := state.Delta.Cursor.Offset
			if err := s.claimInventory(ctx, leader, &state); err != nil {
				t.Fatal(err)
			}
			if state.Delta == nil || state.Delta.Cursor.Offset != offset {
				t.Fatal("takeover discarded incremental source progress")
			}
			stale := state
			if err := s.advanceInventory(ctx, previous, &stale); !errors.Is(err, lease.ErrLost) {
				t.Fatalf("released owner advanced incremental work: %v", err)
			}
			takenOver = true
		}
		var err error
		state, _, err = inventory.Read[inventoryCheckpoint](ctx, s.store, inventory.Prefix+"checkpoint.json")
		if err != nil {
			t.Fatal(err)
		}
		if state.Build == nil && state.LastCompleteAt.After(started) {
			break
		}
		if time.Since(started) > 3*time.Minute || step == 19999 {
			t.Fatalf("historical refresh exceeded three minutes: build=%d incremental=%v", state.BuildPhase, state.Delta != nil)
		}
	}
	if !takenOver {
		t.Fatal("historical refresh did not exercise incremental takeover")
	}
	snapshot, err := s.inventory.Snapshot(ctx, "", time.Minute)
	if err != nil || snapshot.Coverage != "complete" || snapshot.UniqueManifests != 104 {
		t.Fatalf("historical refresh lost content: snapshot=%+v error=%v", snapshot, err)
	}
	assertInventoryTestCounts(t, s, snapshot)
	t.Logf("historical refresh=%s under continued binding writes, checkpoint reload and leader takeover", time.Since(started))
}
