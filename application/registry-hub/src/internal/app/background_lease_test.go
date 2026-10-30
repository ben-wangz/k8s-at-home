package app

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

type deadlineLeaseStore struct {
	storage.Store
	block  atomic.Bool
	calls  atomic.Int64
	writes chan string
}

func (s *deadlineLeaseStore) observe(ctx context.Context) error {
	s.calls.Add(1)
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > backgroundLeaseCallBudget+time.Second {
		return errors.New("lease call has no finite coordination budget")
	}
	if s.block.Load() {
		<-ctx.Done()
		return ctx.Err()
	}
	return ctx.Err()
}

func (s *deadlineLeaseStore) Get(ctx context.Context, key string) (storage.Object, error) {
	if err := s.observe(ctx); err != nil {
		return storage.Object{}, err
	}
	return s.Store.Get(ctx, key)
}

func (s *deadlineLeaseStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if err := s.observe(ctx); err != nil {
		return storage.Entry{}, err
	}
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if err == nil {
		identity, _ := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext)
		s.writes <- identity.Scope
	}
	return entry, err
}

func newDeadlineLeaseStore(t *testing.T) *deadlineLeaseStore {
	t.Helper()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return &deadlineLeaseStore{Store: store, writes: make(chan string, 32)}
}

func TestBackgroundLeaseAdmissionMakesNoStorageCallsOnYield(t *testing.T) {
	store := newDeadlineLeaseStore(t)
	s := &Service{backgroundSlots: make(chan struct{}, 1)}
	s.backgroundSlots <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, _, err := s.acquireBackgroundLease(ctx, "test", lease.NewManager(store), "test", "owner", time.Minute)
	if !errors.Is(err, errBackgroundAdmissionYield) || store.calls.Load() != 0 {
		t.Fatalf("yield started storage work: err=%v calls=%d", err, store.calls.Load())
	}
	if len(s.backgroundSlots) != 1 {
		t.Fatal("yield changed held shared capacity")
	}
}

func TestBackgroundLeaseAcquisitionAndReleaseRespectEarlierDeadline(t *testing.T) {
	for _, stage := range []string{"acquire", "release"} {
		t.Run(stage, func(t *testing.T) {
			store := newDeadlineLeaseStore(t)
			s := &Service{}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			store.block.Store(stage == "acquire")
			_, _, release, err := s.acquireBackgroundLease(ctx, "test", lease.NewManager(store), "test", "owner", time.Minute)
			if stage == "release" {
				if err != nil {
					t.Fatal(err)
				}
				store.block.Store(true)
				err = release()
				calls := store.calls.Load()
				_ = release()
				if store.calls.Load() != calls {
					t.Fatal("repeated release started new storage calls")
				}
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s ignored the earlier deadline: %v", stage, err)
			}
		})
	}
}

func TestBackgroundLeaseRenewalKeepsTTLAndReservedCapacity(t *testing.T) {
	store := newDeadlineLeaseStore(t)
	s := &Service{backgroundSlots: make(chan struct{}, 1)}
	manager := lease.NewManager(store)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const ttl = 600 * time.Millisecond
	leader, holder, release, err := s.acquireBackgroundLease(ctx, "test", manager, "test", "owner", ttl)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	<-store.writes
	s.backgroundSlots <- struct{}{}
	select {
	case scope := <-store.writes:
		if scope != "lease_renewal" {
			t.Fatalf("renewal scope=%q", scope)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("renewal blocked behind bulk admission")
	}
	state, _, err := inventory.Read[struct{ ExpiresAt time.Time }](context.Background(), store.Store, holder.Key)
	if err != nil || time.Until(state.ExpiresAt) > ttl || leader.Err() != nil {
		t.Fatalf("renewal changed TTL or lost ownership: state=%+v err=%v leader=%v", state, err, leader.Err())
	}
	if err := release(); err != nil || len(s.backgroundSlots) != 1 {
		t.Fatalf("reserved release failed: err=%v slots=%d", err, len(s.backgroundSlots))
	}
}

func TestBackgroundLeaseRenewalTimeoutCancelsLeader(t *testing.T) {
	store := newDeadlineLeaseStore(t)
	s := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leader, _, release, err := s.acquireBackgroundLease(ctx, "test", lease.NewManager(store), "test", "owner", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	store.block.Store(true)
	select {
	case <-leader.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stalled lease renewal did not cancel leadership")
	}
	store.block.Store(false)
	if err := release(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("renewal error was lost: %v", err)
	}
}

func TestBackgroundLeaseShutdownStartsNoDetachedRelease(t *testing.T) {
	store := newDeadlineLeaseStore(t)
	s := &Service{}
	ctx, cancel := context.WithCancel(context.Background())
	_, _, release, err := s.acquireBackgroundLease(ctx, "test", lease.NewManager(store), "test", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	calls := store.calls.Load()
	if err := release(); err != nil || store.calls.Load() != calls {
		t.Fatalf("shutdown started detached storage work: err=%v calls=%d", err, store.calls.Load()-calls)
	}
}

func TestBackgroundLeaseUnitCancellationCanReleaseWithinLifecycle(t *testing.T) {
	store := newDeadlineLeaseStore(t)
	manager := lease.NewManager(store)
	lifecycle, stop := context.WithCancel(context.Background())
	defer stop()
	unit, cancel := context.WithCancel(lifecycle)
	defer cancel()
	unit = context.WithValue(unit, backgroundParentKey{}, lifecycle)
	_, _, release, err := acquireRenewedLease(unit, manager, "test", "owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-unit.Done()
	if err := release(); err != nil {
		t.Fatalf("unit cancellation prevented bounded release: %v", err)
	}
	active, err := lease.NewManager(store.Store).Active(context.Background(), "test")
	if err != nil || active {
		t.Fatalf("expired execution left a live lease: active=%t err=%v", active, err)
	}
}
