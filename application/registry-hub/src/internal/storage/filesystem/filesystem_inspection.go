package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"registry-hub/internal/storage"
	"syscall"
)

func (s *Store) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, "", err
	}
	path, err := s.path(key)
	if err != nil {
		return storage.Entry{}, "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return storage.Entry{}, "", storage.ErrNotFound
	}
	if err != nil {
		return storage.Entry{}, "", mapError(err)
	}
	if !info.Mode().IsRegular() {
		return storage.Entry{}, "", storage.ErrCorruptObject
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return storage.Entry{}, "", storage.ErrUnavailable
	}
	fingerprint := fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d", stat.Dev, stat.Ino, stat.Size, stat.Mtim.Sec, stat.Mtim.Nsec, stat.Ctim.Sec, stat.Ctim.Nsec)
	return storage.Entry{Key: key, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, fingerprint, nil
}

func (s *Store) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	return s.scanMetadata(ctx, prefix, token, limit)
}
