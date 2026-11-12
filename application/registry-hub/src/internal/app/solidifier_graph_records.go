package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

type graphEpochKey struct{}

func (s *solidifier) checkGraphEpoch(ctx context.Context) error {
	if epoch, ok := ctx.Value(graphEpochKey{}).(uint64); ok && !s.stagedManifestObservationEpochIsCurrent(epoch) {
		return tasks.WaitFor(tasks.WaitingDeps, "gc_epoch_changed")
	}
	return nil
}

func saveGraphRecord[T any](ctx context.Context, store storage.Store, key string, value T) error {
	err := inventory.Update(ctx, store, key, func(current *T) error { *current = value; return nil })
	if !errors.Is(err, storage.ErrCorruptObject) {
		return err
	}
	// Optional acceleration state can be rebuilt from validated discovery after corruption.
	object, err := store.Get(ctx, key)
	if err != nil {
		return err
	}
	if err := object.Body.Close(); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: object.Version})
	if errors.Is(err, storage.ErrUnknownCommit) {
		actual, _, readErr := inventory.Read[T](ctx, store, key)
		encoded, _ := json.Marshal(actual)
		if readErr == nil && bytes.Equal(encoded, data) {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}
