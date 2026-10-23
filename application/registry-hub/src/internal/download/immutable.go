package download

import (
	"bytes"
	"context"
	"errors"
	"io"

	"registry-hub/internal/storage"
)

func (m *Manager) putImmutable(ctx context.Context, key string, data []byte, stage string) (storage.Entry, error) {
	entry, err := m.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrUnknownCommit) {
		if err != nil {
			return storage.Entry{}, persistenceError(stage, err)
		}
		if entry.Version == "" {
			return storage.Entry{}, persistenceError(stage, errors.Join(storage.ErrUnknownCommit, storage.ErrCorruptObject))
		}
		return entry, nil
	}
	object, readErr := m.store.Get(ctx, key)
	if readErr != nil {
		return storage.Entry{}, persistenceError(stage+"_confirm", errors.Join(err, readErr))
	}
	defer object.Body.Close()
	stored, readErr := io.ReadAll(io.LimitReader(object.Body, int64(len(data))+1))
	if readErr == nil && object.Version != "" && object.Size == int64(len(data)) && bytes.Equal(stored, data) {
		return storage.Entry{Key: key, Version: object.Version, Size: object.Size, ModTime: object.ModTime}, nil
	}
	if readErr != nil {
		return storage.Entry{}, persistenceError(stage+"_confirm", errors.Join(err, readErr))
	}
	return storage.Entry{}, persistenceError(stage+"_confirm", storage.ErrUnknownCommit)
}
