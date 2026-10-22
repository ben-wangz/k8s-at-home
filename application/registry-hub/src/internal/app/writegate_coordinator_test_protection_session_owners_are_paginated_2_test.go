package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"sync"
	"testing"
)

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
