package filesystem

import (
	"context"
	"io"
	"registry-hub/internal/storage"
)

func (s *Store) Head(ctx context.Context, key string) (storage.Entry, error) {
	obj, err := s.Get(ctx, key)
	if err != nil {
		return storage.Entry{}, err
	}
	_ = obj.Body.Close()
	return storage.Entry{Key: key, Version: obj.Version, Size: obj.Size, ModTime: obj.ModTime}, nil
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options, "")
}

func (s *Store) PutVerified(ctx context.Context, key string, body io.Reader, size int64, options storage.VerifiedPutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options.PutOptions, options.Digest)
}
