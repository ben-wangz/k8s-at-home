package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"sync"
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

func TestProtectionSessionOwnersArePaginated(t *testing.T) {
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
	owners := []string{mustGateTestID(t), mustGateTestID(t), mustGateTestID(t)}
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:abc", owners[0], 1024, 4, 4096); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners[1:] {
		if err := gate.AddProtectionOwner(ctx, sessionID, owner); err != nil {
			t.Fatal(err)
		}
	}
	first, token, err := gate.ListProtectionSessionOwners(ctx, sessionID, "", 2)
	if err != nil || len(first) != 2 || token == "" {
		t.Fatalf("first owner page = %v token=%q err=%v", first, token, err)
	}
	second, next, err := gate.ListProtectionSessionOwners(ctx, sessionID, token, 2)
	if err != nil || len(second) != 1 || next != "" {
		t.Fatalf("second owner page = %v token=%q err=%v", second, next, err)
	}
	seen := map[string]bool{}
	for _, owner := range append(first, second...) {
		seen[owner] = true
	}
	for _, owner := range owners {
		if !seen[owner] {
			t.Fatalf("paginated owner list omitted %s", owner)
		}
	}
}

func TestProtectionAndGCAdmissionRaceHasOneDurableWinner(t *testing.T) {
	for iteration := range 20 {
		t.Run(fmt.Sprintf("race_%02d", iteration), func(t *testing.T) {
			ctx := context.Background()
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			admissionGate, err := newWriteGate(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			gcGate, err := newWriteGate(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			sessionID := mustGateTestID(t)
			start := make(chan struct{})
			var workers sync.WaitGroup
			var admitErr, gcErr error
			workers.Add(2)
			go func() {
				defer workers.Done()
				<-start
				admitErr = admissionGate.AdmitProtection(ctx, sessionID, "scope", "", 128, 4, 1024)
			}()
			go func() {
				defer workers.Done()
				<-start
				gcErr = gcGate.BeginDrain(ctx)
			}()
			close(start)
			workers.Wait()
			if (admitErr == nil) == (gcErr == nil) {
				t.Fatalf("admission error=%v GC error=%v; exactly one CAS should win", admitErr, gcErr)
			}
			current, err := newWriteGate(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			state := current.State()
			if admitErr == nil {
				if state.State != "open" || state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 128 {
					t.Fatalf("durable admission winner state = %#v", state)
				}
			} else if state.State != "draining" || state.ProtectionSessionCount != 0 || state.ReservedDistributionBytes != 0 {
				t.Fatalf("durable GC winner state = %#v", state)
			}
		})
	}
}

func TestUnknownCoordinatorAndOwnerCommitsRetainGCProtection(t *testing.T) {
	ctx := context.Background()
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &commitThenUnknownStore{Store: base, unknownKeys: make(map[string]bool)}
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := mustGateTestID(t)
	firstOwner := mustGateTestID(t)
	secondOwner := mustGateTestID(t)
	store.unknownOnce("system/gc/state.json")
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:root", firstOwner, 512, 4, 4096); err != nil {
		t.Fatalf("admit protection after committed unknown coordinator write: %v", err)
	}
	if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission after coordinator readback = %v, want protected", err)
	}

	store.unknownOnce(protectionOwnerKey(sessionID, secondOwner))
	if err := gate.AddProtectionOwner(ctx, sessionID, secondOwner); err != nil {
		t.Fatalf("add owner after committed unknown owner write: %v", err)
	}
	owners, err := gate.ProtectionSessionOwners(ctx, sessionID)
	if err != nil || len(owners) != 2 || !containsString(owners, firstOwner) || !containsString(owners, secondOwner) {
		t.Fatalf("protection owners after owner readback = %v err=%v, want both owners", owners, err)
	}
	if err := gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission after owner readback = %v, want protected", err)
	}

	restarted, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if state := restarted.State(); state.ProtectionSessionCount != 1 || state.ReservedDistributionBytes != 512 {
		t.Fatalf("coordinator after restart = %#v, want retained session and reservation", state)
	}
	if err := restarted.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC admission after restart = %v, want protected", err)
	}
}

type commitThenUnknownStore struct {
	storage.Store
	mu          sync.Mutex
	unknownKeys map[string]bool
}

func (s *commitThenUnknownStore) unknownOnce(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unknownKeys[key] = true
}

func (s *commitThenUnknownStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err != nil {
		return entry, err
	}
	s.mu.Lock()
	unknown := s.unknownKeys[key]
	delete(s.unknownKeys, key)
	s.mu.Unlock()
	if unknown {
		return storage.Entry{}, storage.ErrUnknownCommit
	}
	return entry, nil
}

func TestGCAdmissionFailsClosedOnInconsistentProtectionLedger(t *testing.T) {
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
	gate.state.LegacyGrandfatheredBytes = 1
	before := gate.State()
	if err := gate.BeginDrain(ctx); err == nil {
		t.Fatal("GC admission accepted an inconsistent legacy reservation total")
	}
	if state := gate.State(); state.State != before.State || state.Epoch != before.Epoch {
		t.Fatalf("gate after rejected GC admission = %#v, want unchanged open state %#v", state, before)
	}
}

func mustGateTestID(t *testing.T) string {
	t.Helper()
	id, err := randomRunID()
	if err != nil {
		t.Fatal(err)
	}
	if !validGateID(id) || strings.TrimSpace(id) != id {
		t.Fatalf("generated invalid gate ID %q", id)
	}
	return id
}

func assertGateStateSchema(t *testing.T, store *filesystem.Store, expected int) {
	t.Helper()
	object, err := store.Get(context.Background(), "system/gc/state.json")
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	var state struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.NewDecoder(object.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.SchemaVersion != expected {
		t.Fatalf("gate schema version = %d, want %d", state.SchemaVersion, expected)
	}
}
