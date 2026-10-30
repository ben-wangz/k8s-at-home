package app

import (
	"context"
	"io"
	"registry-hub/internal/storage"
)

type inspectionReadOnlyStore struct{ storage.Store }

func (s inspectionReadOnlyStore) Put(context.Context, string, io.Reader, int64, storage.PutOptions) (storage.Entry, error) {
	return storage.Entry{}, storage.ErrPermissionDenied
}

func (s inspectionReadOnlyStore) Delete(context.Context, string) error {
	return storage.ErrPermissionDenied
}

func (s inspectionReadOnlyStore) DeleteIfMatch(context.Context, string, string) error {
	return storage.ErrPermissionDenied
}

func (s inspectionReadOnlyStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	return storage.Scan(ctx, s.Store, prefix, token, limit)
}
