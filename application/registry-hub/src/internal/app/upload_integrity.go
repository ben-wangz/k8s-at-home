package app

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

type uploadIntegrityReader struct{ inner *storage.IntegrityReader }

func newUploadIntegrityReader(ctx context.Context, reader io.Reader, digest string, size int64) (*uploadIntegrityReader, error) {
	inner, err := storage.NewIntegrityReader(ctx, reader, digest, size, size)
	if err != nil {
		return nil, err
	}
	return &uploadIntegrityReader{inner: inner}, nil
}

func (r *uploadIntegrityReader) Read(buffer []byte) (int, error) { return r.inner.Read(buffer) }

func (r *uploadIntegrityReader) Finalize() error {
	err := r.inner.Finalize()
	if errors.Is(err, storage.ErrDigestMismatch) || errors.Is(err, storage.ErrSizeExceeded) {
		return errors.Join(artifact.ErrDigestMismatch, err)
	}
	return err
}
