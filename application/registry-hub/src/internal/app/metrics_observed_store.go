package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"registry-hub/internal/storage"
	"strings"
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
	callCtx := attributedStorageContext(ctx, "get", key)
	ioCtx := storage.WithIOObserver(callCtx, s.ObserveIO)
	object, err := s.Store.Get(ioCtx, key)
	if err == nil {
		object.Body = storage.MeasureBody(ioCtx, object.Body)
	}
	recordInventoryDependency(ctx, key, object.Version, err)
	s.record(callCtx, "get", err, started)
	return object, err
}

func (s *observedStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "head", key)
	entry, err := s.Store.Head(storage.WithIOObserver(callCtx, s.ObserveIO), key)
	recordInventoryDependency(ctx, key, entry.Version, err)
	s.record(callCtx, "head", err, started)
	return entry, err
}

func (s *observedStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "put", key)
	source := &storage.CountingReader{Reader: body}
	entry, err := s.Store.Put(storage.WithIOObserver(callCtx, s.ObserveIO), key, source, size, options)
	s.ObserveIO(ctx, "write", source.Count, time.Since(started))
	if err == nil {
		s.metrics.recordInventorySnapshotWrite(ctx, key, source.Count)
	}
	s.record(callCtx, "put", err, started)
	s.inventoryChanged(ctx, key, entry.Version, err)
	return entry, err
}

func (s *observedStore) PutVerified(ctx context.Context, key string, body io.Reader, size int64, options storage.VerifiedPutOptions) (storage.Entry, error) {
	source, ok := s.Store.(storage.VerifiedPutter)
	if !ok {
		return storage.Entry{}, errors.ErrUnsupported
	}
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "put_verified", key)
	reader := &storage.CountingReader{Reader: body}
	entry, err := source.PutVerified(storage.WithIOObserver(callCtx, s.ObserveIO), key, reader, size, options)
	s.ObserveIO(ctx, "write", reader.Count, time.Since(started))
	s.record(callCtx, "put_verified", err, started)
	if s.metrics != nil && strings.Contains(key, "/data") {
		if errors.Is(err, storage.ErrDigestMismatch) {
			s.metrics.recordEvent("integrity_write_mismatch")
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			s.metrics.recordEvent("integrity_unknown_commit")
		} else if err == nil {
			s.metrics.recordEvent("integrity_write_verified")
		}
	}
	s.inventoryChanged(ctx, key, entry.Version, err)
	return entry, err
}

func (s *observedStore) Delete(ctx context.Context, key string) error {
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "delete", key)
	err := s.Store.Delete(callCtx, key)
	s.record(callCtx, "delete", err, started)
	s.inventoryChanged(ctx, key, "missing", err)
	return err
}

func (s *observedStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "delete_if_match", key)
	err := s.Store.DeleteIfMatch(storage.WithIOObserver(callCtx, s.ObserveIO), key, version)
	s.record(callCtx, "delete_if_match", err, started)
	s.inventoryChanged(ctx, key, "missing", err)
	return err
}

func (s *observedStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	started := time.Now()
	callCtx := attributedStorageContext(ctx, "list", prefix)
	entries, next, err := s.Store.List(storage.WithIOObserver(callCtx, s.ObserveIO), prefix, token, limit)
	recordInventoryListing(ctx, prefix, token, limit, entries, next, err, false)
	s.record(callCtx, "list", err, started)
	return entries, next, err
}

func (s *observedStore) Close() error {
	started := time.Now()
	err := s.Store.Close()
	s.record(context.Background(), "close", err, started)
	return err
}
