package filesystem

import (
	"context"
	"errors"
	"os"
	"registry-hub/internal/storage"
	"syscall"
)

type payloadReader struct {
	*os.File
	fingerprint string
}

func (r *payloadReader) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := r.Stat()
	if err != nil {
		return mapError(err)
	}
	fingerprint, err := fileFingerprint(info)
	if err != nil {
		return err
	}
	if fingerprint != r.fingerprint {
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Store) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	if err := ctx.Err(); err != nil {
		return storage.Payload{}, err
	}
	path, err := s.path(key)
	if err != nil {
		return storage.Payload{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return storage.Payload{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.Payload{}, mapError(err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return storage.Payload{}, mapError(err)
	}
	fingerprint, err := fileFingerprint(info)
	if err != nil {
		_ = file.Close()
		return storage.Payload{}, err
	}
	return storage.Payload{Body: &payloadReader{File: file, fingerprint: fingerprint}, Size: info.Size(), Fingerprint: fingerprint}, nil
}
