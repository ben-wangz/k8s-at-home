package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type storageReadCacheKey struct{}
type storageReadValue struct {
	object storage.Object
	data   []byte
}
type storageReadCache struct {
	values map[string]storageReadValue
	bytes  int
}

func newStorageReadCache() *storageReadCache {
	return &storageReadCache{values: map[string]storageReadValue{}}
}

type storageMetadataReader struct {
	storage.Store
	cache *storageReadCache
}

func (s storageMetadataReader) Get(ctx context.Context, key string) (storage.Object, error) {
	if err := ctx.Err(); err != nil {
		return storage.Object{}, err
	}
	if value, ok := s.cache.values[key]; ok {
		object := value.object
		object.Body = io.NopCloser(bytes.NewReader(value.data))
		return object, nil
	}
	object, err := s.Store.Get(ctx, key)
	if err != nil || object.Size < 0 || object.Size > 1<<20 || s.cache.bytes+int(object.Size) > 4<<20 || len(s.cache.values) >= 128 {
		return object, err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return storage.Object{}, err
	}
	if len(data) > 1<<20 {
		return storage.Object{}, storage.ErrCorruptObject
	}
	object.Body = nil
	s.cache.values[key] = storageReadValue{object: object, data: data}
	s.cache.bytes += len(data)
	object.Body = io.NopCloser(bytes.NewReader(data))
	return object, nil
}

func storageCollectionRead[T any](ctx context.Context, store storage.Store, key string) (T, string, error) {
	if cache, ok := ctx.Value(storageReadCacheKey{}).(*storageReadCache); ok {
		store = storageMetadataReader{Store: store, cache: cache}
	}
	return inventory.Read[T](ctx, store, key)
}
