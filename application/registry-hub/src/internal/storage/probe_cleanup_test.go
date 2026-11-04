package storage_test

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

type probeCleanupStore struct {
	storage.Store
	cancel         context.CancelFunc
	puts           int
	cleanupCalled  bool
	cleanupContext error
	cleanupError   error
	cleanupBounded bool
}

func (s *probeCleanupStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.puts++
	if s.puts == 2 {
		s.cancel()
		return storage.Entry{}, storage.ErrTimeout
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *probeCleanupStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.cleanupCalled, s.cleanupContext = true, ctx.Err()
	deadline, ok := ctx.Deadline()
	s.cleanupBounded = ok && time.Until(deadline) <= 5*time.Second
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.cleanupError != nil {
		return s.cleanupError
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestProbeCleanupRetainsBothErrorsAndUsesLifecycleBudget(t *testing.T) {
	for _, lifecycleCancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "execution_deadline", true: "service_shutdown"}[lifecycleCancelled], func(t *testing.T) {
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = base.Close() })
			parent, stop := context.WithCancel(context.Background())
			defer stop()
			execution, cancel := context.WithCancel(parent)
			defer cancel()
			store := &probeCleanupStore{Store: base, cancel: cancel, cleanupError: storage.ErrPermissionDenied}
			if lifecycleCancelled {
				store.cancel = stop
			}
			err = storage.ProbeWithCleanupContext(execution, parent, store)
			if !errors.Is(err, storage.ErrTimeout) || !strings.Contains(err.Error(), "conditional create") || !strings.Contains(err.Error(), "auxiliary cleanup") || !store.cleanupCalled || !store.cleanupBounded {
				t.Fatalf("probe lost stage, cause, or cleanup budget: %v %+v", err, store)
			}
			want := storage.ErrPermissionDenied
			if lifecycleCancelled {
				want = context.Canceled
			} else if store.cleanupContext != nil {
				t.Fatalf("cleanup reused expired execution context: %v", store.cleanupContext)
			}
			if !errors.Is(err, want) {
				t.Fatalf("cleanup cause missing: %v", err)
			}
		})
	}
}
