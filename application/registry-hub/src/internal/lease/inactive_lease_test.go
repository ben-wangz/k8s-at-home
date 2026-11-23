package lease

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

type leaseDeleteRaceStore struct {
	storage.Store
	beforeDelete func(context.Context) error
	delayPut     func(func() (storage.Entry, error)) (storage.Entry, error)
}

func (s *leaseDeleteRaceStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if s.delayPut == nil {
		return s.Store.Put(ctx, key, body, size, options)
	}
	return s.delayPut(func() (storage.Entry, error) { return s.Store.Put(ctx, key, body, size, options) })
}

func (s *leaseDeleteRaceStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if s.beforeDelete != nil {
		beforeDelete := s.beforeDelete
		s.beforeDelete = nil
		if err := beforeDelete(ctx); err != nil {
			return err
		}
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestDeleteInactiveLeasePreservesLiveLeaseAndDeletesExpiredHeldLease(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager := NewManager(store)
	ctx := context.Background()
	active, err := manager.Acquire(ctx, "diagnostics/event-outbox/active", "active-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := manager.DeleteInactive(ctx, active.Key)
	if err != nil || deleted {
		t.Fatalf("delete active lease = %v, %v; want false, nil", deleted, err)
	}
	if err := manager.Validate(ctx, active); err != nil {
		t.Fatalf("active lease did not survive cleanup: %v", err)
	}

	expired, err := manager.Acquire(ctx, "diagnostics/event-outbox/interrupted", "interrupted-owner", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(expired.ExpiresAt); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}
	deleted, err = manager.DeleteInactive(ctx, expired.Key)
	if err != nil || !deleted {
		t.Fatalf("delete expired lease = %v, %v; want true, nil", deleted, err)
	}
	if _, err := store.Head(ctx, expired.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired lease remains: %v", err)
	}
}

func TestDeleteInactiveLeaseSkipsConcurrentTakeover(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &leaseDeleteRaceStore{Store: base}
	manager := NewManager(store)
	ctx := context.Background()
	key := "diagnostics/event-outbox/takeover-race"
	expired, err := manager.Acquire(ctx, key, "expired-owner", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if delay := time.Until(expired.ExpiresAt); delay > 0 {
		time.Sleep(delay + time.Millisecond)
	}
	var successor Lease
	store.beforeDelete = func(ctx context.Context) error {
		var acquireErr error
		successor, acquireErr = manager.Acquire(ctx, key, "successor-owner", time.Minute)
		return acquireErr
	}
	deleted, err := manager.DeleteInactive(ctx, expired.Key)
	if err != nil || deleted {
		t.Fatalf("cleanup racing takeover = %v, %v; want skipped deletion", deleted, err)
	}
	if err := manager.Validate(ctx, successor); err != nil {
		t.Fatalf("takeover lease was deleted or invalidated: %v", err)
	}
	if err := manager.Validate(ctx, expired); !errors.Is(err, ErrLost) {
		t.Fatalf("expired holder validation = %v, want ErrLost", err)
	}
}

func TestDeleteInactiveLeaseSkipsConcurrentRenewal(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	synctest.Test(t, func(t *testing.T) {
		store := &leaseDeleteRaceStore{Store: base}
		manager := NewManager(store)
		ctx := context.Background()
		active, err := manager.Acquire(ctx, "diagnostics/event-outbox/renewal-race", "active-owner", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		store.delayPut = func(write func() (storage.Entry, error)) (storage.Entry, error) {
			// Let cleanup read the expired record while the renewal write is in flight.
			time.Sleep(time.Until(active.ExpiresAt))
			var entry storage.Entry
			store.beforeDelete = func(context.Context) error {
				var err error
				entry, err = write()
				return err
			}
			deleted, err := manager.DeleteInactive(ctx, active.Key)
			if err != nil || deleted || entry.Version == "" {
				t.Fatalf("cleanup racing renewal = %v, %v; want skipped deletion after the renewal write", deleted, err)
			}
			return entry, nil
		}
		renewed, err := manager.Renew(ctx, active, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Validate(ctx, renewed); err != nil {
			t.Fatalf("renewed lease did not survive cleanup: %v", err)
		}
	})
}
