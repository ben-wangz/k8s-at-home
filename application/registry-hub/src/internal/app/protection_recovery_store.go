package app

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errRecoveryPaused = errors.New("protection metadata mutations are paused for recovery")

type recoveryWriteGuardKey struct{}
type recoveryWriteGuard func(context.Context) error

// The supported single backend freezes source mutations while its recovery cursor advances.
type recoveryStore struct {
	storage.Store
	blocked   atomic.Bool
	mutations sync.RWMutex
}

func recoverySourceKey(key string) bool {
	for _, prefix := range []string{"tasks/", "solidification/", "cache/", "downloads/", "system/gc/", "system/solidification/"} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (s *recoveryStore) mutation(ctx context.Context, key string) (func(), error) {
	if !recoverySourceKey(key) {
		return func() {}, nil
	}
	s.mutations.RLock()
	if guard, ok := ctx.Value(recoveryWriteGuardKey{}).(recoveryWriteGuard); ok {
		if err := guard(ctx); err != nil {
			s.mutations.RUnlock()
			return nil, err
		}
	} else if s.blocked.Load() {
		s.mutations.RUnlock()
		return nil, errRecoveryPaused
	}
	return s.mutations.RUnlock, nil
}

func (s *recoveryStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	release, err := s.mutation(ctx, key)
	if err != nil {
		return storage.Entry{}, err
	}
	defer release()
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *recoveryStore) Delete(ctx context.Context, key string) error {
	release, err := s.mutation(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	return s.Store.Delete(ctx, key)
}

func (s *recoveryStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	release, err := s.mutation(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func (s *recoveryStore) quiesce(ctx context.Context) error {
	s.blocked.Store(true)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.mutations.TryLock() {
			s.mutations.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *recoveryStore) ReplayUsage() (int64, int64, int) {
	if provider, ok := s.Store.(interface{ ReplayUsage() (int64, int64, int) }); ok {
		return provider.ReplayUsage()
	}
	return 0, 0, 0
}

func (s *recoveryStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	return storage.Scan(ctx, s.Store, prefix, token, limit)
}

func (s *recoveryStore) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	if source, ok := s.Store.(storage.Fingerprinter); ok {
		return source.Fingerprint(ctx, key)
	}
	return storage.Entry{}, "", nil
}

func (s *recoveryStore) SetHTTPAttemptObserver(observer storage.HTTPAttemptObserver) {
	if source, ok := s.Store.(interface {
		SetHTTPAttemptObserver(storage.HTTPAttemptObserver)
	}); ok {
		source.SetHTTPAttemptObserver(observer)
	}
}

func (s *recoveryStore) CleanupOrphans(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	s.mutations.RLock()
	defer s.mutations.RUnlock()
	if s.blocked.Load() {
		return 0, errRecoveryPaused
	}
	if cleaner, ok := s.Store.(interface {
		CleanupOrphans(context.Context, time.Duration, int) (int, error)
	}); ok {
		return cleaner.CleanupOrphans(ctx, olderThan, limit)
	}
	return 0, errors.New("storage backend does not support orphan cleanup")
}

func (s *recoveryStore) CleanupOrphansStep(ctx context.Context, olderThan time.Duration, token string) (string, int, error) {
	s.mutations.RLock()
	defer s.mutations.RUnlock()
	if s.blocked.Load() {
		return token, 0, errRecoveryPaused
	}
	if cleaner, ok := s.Store.(interface {
		CleanupOrphansStep(context.Context, time.Duration, string) (string, int, error)
	}); ok {
		return cleaner.CleanupOrphansStep(ctx, olderThan, token)
	}
	return "", 0, nil
}

func (g *writeGate) recoveryMutationAllowed(ctx context.Context) error {
	if g.recovery != nil && g.recovery.blocked.Load() {
		if _, ok := ctx.Value(recoveryWriteGuardKey{}).(recoveryWriteGuard); !ok {
			return errRecoveryPaused
		}
	}
	return nil
}
