package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestProtectionSessionBlocksGCUntilUnownedAndQuiesced(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := mustGateTestID(t)
	firstDemand := mustGateTestID(t)
	secondDemand := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:abc", firstDemand, 4096, 4, 8192); err != nil {
		t.Fatal(err)
	}
	state := gate.State()
	if state.State != "open" || state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 4096 || len(state.SessionIDs) != 0 {
		t.Fatalf("public gate snapshot = %#v", state)
	}
	if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission with active protection = %v, want ErrGCProtected", err)
	}
	if err := gate.AddProtectionOwner(ctx, sessionID, secondDemand); err != nil {
		t.Fatal(err)
	}
	releaseOperation, err := gate.BeginProtectionOperation(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if owners, err := gate.RemoveProtectionOwner(ctx, sessionID, firstDemand); err != nil || owners != 1 {
		t.Fatalf("remaining owners after first cancellation = %d err=%v, want 1", owners, err)
	}
	if stopping, err := gate.RequestProtectionStop(ctx, sessionID, "cancelled"); err != nil || stopping {
		t.Fatalf("shared session stop = %t err=%v, want it to remain active", stopping, err)
	}
	if owners, err := gate.RemoveProtectionOwner(ctx, sessionID, secondDemand); err != nil || owners != 0 {
		t.Fatalf("remaining owners after last cancellation = %d err=%v, want 0", owners, err)
	}
	if stopping, err := gate.RequestProtectionStop(ctx, sessionID, "cancelled"); err != nil || !stopping {
		t.Fatalf("unowned session stop = %t err=%v, want stop requested", stopping, err)
	}
	if err := gate.MarkProtectionQuiescing(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := gate.MarkProtectionQuiesced(ctx, sessionID); err == nil {
		t.Fatal("session became quiesced while an operation remained active")
	}
	if err := releaseOperation(ctx); err != nil {
		t.Fatal(err)
	}
	if err := gate.MarkProtectionQuiesced(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReleaseProtection(ctx, sessionID, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if state := gate.State(); state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 || state.State != "open" {
		t.Fatalf("gate after safe cancellation = %#v", state)
	}
	if err := gate.BeginDrain(ctx); err != nil {
		t.Fatalf("GC admission after protection release: %v", err)
	}
}

func TestAnchoredProtectionCanBeReopenedAfterConfirmedMiss(t *testing.T) {
	for _, released := range []bool{false, true} {
		name := "active"
		if released {
			name = "released"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			gate, err := newWriteGate(ctx, store)
			if err != nil {
				t.Fatal(err)
			}

			sessionID := mustGateTestID(t)
			previousDemand := mustGateTestID(t)
			currentDemand := mustGateTestID(t)
			if err := gate.AdmitProtection(ctx, sessionID, "manifest-scope", previousDemand, 4096, 4, 8192); err != nil {
				t.Fatal(err)
			}
			if err := gate.MarkProtectionContentObserved(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if released {
				if err := gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
					t.Fatal(err)
				}
			}
			if err := gate.ReopenAnchoredProtection(ctx, sessionID, "manifest-scope", currentDemand, 4096, 4, 8192); err != nil {
				t.Fatalf("reopen anchored scope after a confirmed miss: %v", err)
			}

			state := gate.State()
			if state.ProtectionSessionCount != 1 || state.NewProtectionSessionCount != 1 || state.ReservedDistributionBytes != 4096 {
				t.Fatalf("gate after reopening anchored scope = %#v", state)
			}
			sessions, _, err := gate.ListProtectionSessions(ctx, "", 4)
			if err != nil || len(sessions) != 1 || sessions[0].State != "protected" || !sessions[0].MayContainUnanchored || sessions[0].OwnerCount != 2 {
				t.Fatalf("reopened session = %#v err=%v", sessions, err)
			}
			if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
				t.Fatalf("GC admission after reopening = %v, want ErrGCProtected", err)
			}
			if err := gate.MarkProtectionAnchored(ctx, sessionID); err != nil {
				t.Fatal(err)
			}
			if err := gate.ReleaseProtection(ctx, sessionID, "anchored"); err != nil {
				t.Fatal(err)
			}
			if err := gate.BeginDrain(ctx); err != nil {
				t.Fatalf("GC admission after repaired scope anchors: %v", err)
			}
		})
	}
}

func TestProtectionTransferReplacesReservationAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	oldID := mustGateTestID(t)
	newID := mustGateTestID(t)
	demandID := mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, oldID, "blob:sha256:layer", demandID, 6000, 1, 8000); err != nil {
		t.Fatal(err)
	}
	if err := gate.AdmitProtection(ctx, mustGateTestID(t), "other", "", 3000, 2, 8000); !errors.Is(err, ErrProtectionCapacity) {
		t.Fatalf("over-quota protection admission = %v, want ErrProtectionCapacity", err)
	}
	if err := gate.ReplaceProtection(ctx, oldID, newID, "platform:sha256:manifest", 8000, 1, 8000); err != nil {
		t.Fatal(err)
	}
	state := gate.State()
	if state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 8000 {
		t.Fatalf("gate after scope transfer = %#v", state)
	}
	sessions, next, err := gate.ListProtectionSessions(ctx, "", 1)
	if err != nil || next != "" || len(sessions) != 1 || sessions[0].ID != newID || sessions[0].ScopeID != "platform:sha256:manifest" || sessions[0].OwnerCount != 1 {
		t.Fatalf("transferred session page = %#v next=%q err=%v", sessions, next, err)
	}
	oldSession, err := gate.readProtectionSession(ctx, oldID)
	if err != nil || oldSession.State != "transferred" {
		t.Fatalf("old session after transfer = %#v err=%v", oldSession, err)
	}
	if err := gate.MarkProtectionAnchored(ctx, newID); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReleaseProtection(ctx, newID, "anchored"); err != nil {
		t.Fatal(err)
	}
	if state := gate.State(); state.ReservedDistributionBytes != 0 || state.ProtectionSessionCount != 0 {
		t.Fatalf("gate after anchor = %#v", state)
	}
}
