package app

import (
	"context"
	"io"
	"log/slog"
	"registry-hub/internal/storage"
	"time"
)

func newObservedStore(store storage.Store, metrics *metrics, logger *slog.Logger) *observedStore {
	return &observedStore{Store: store, metrics: metrics, logger: logger}
}

func (s *observedStore) ReplayUsage() (int64, int64, int) {
	provider, ok := s.Store.(interface{ ReplayUsage() (int64, int64, int) })
	if !ok {
		return 0, 0, 0
	}
	return provider.ReplayUsage()
}

func (s *observedStore) Get(ctx context.Context, key string) (storage.Object, error) {
	started := time.Now()
	object, err := s.Store.Get(ctx, key)
	s.record("get", err, started)
	return object, err
}

func (s *observedStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	started := time.Now()
	entry, err := s.Store.Head(ctx, key)
	s.record("head", err, started)
	return entry, err
}

func (s *observedStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	started := time.Now()
	entry, err := s.Store.Put(ctx, key, body, size, options)
	s.record("put", err, started)
	return entry, err
}

func (s *observedStore) Delete(ctx context.Context, key string) error {
	started := time.Now()
	err := s.Store.Delete(ctx, key)
	s.record("delete", err, started)
	return err
}

func (s *observedStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	started := time.Now()
	err := s.Store.DeleteIfMatch(ctx, key, version)
	s.record("delete_if_match", err, started)
	return err
}

func (s *observedStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	started := time.Now()
	entries, next, err := s.Store.List(ctx, prefix, token, limit)
	s.record("list", err, started)
	return entries, next, err
}

func (s *observedStore) Close() error {
	started := time.Now()
	err := s.Store.Close()
	s.record("close", err, started)
	return err
}
