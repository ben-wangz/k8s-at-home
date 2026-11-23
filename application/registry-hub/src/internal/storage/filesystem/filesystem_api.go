package filesystem

import (
	"context"
	"io"
	"registry-hub/internal/storage"
)

func (s *Store) Head(ctx context.Context, key string) (result storage.Entry, returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	path, err := s.path(key)
	if err != nil {
		return storage.Entry{}, err
	}
	return s.head(ctx, path, key)
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options, "")
}

func (s *Store) PutVerified(ctx context.Context, key string, body io.Reader, size int64, options storage.VerifiedPutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options.PutOptions, options.Digest)
}
