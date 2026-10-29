package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) RecordManifestObservation(ctx context.Context, task Task, at time.Time, contentGeneration uint64) (Task, error) {
	for attempt := 0; attempt < 16; attempt++ {
		current, err := m.Get(ctx, task.ID)
		if err != nil {
			return Task{}, err
		}
		if current.Generation != task.Generation || current.RootDigest != task.RootDigest {
			return Task{}, storage.ErrConditionFailed
		}
		if !current.ManifestObservedAt.Before(at) {
			return current, nil
		}
		current.ManifestObservedAt, current.ManifestGeneration = at, contentGeneration
		err = m.save(ctx, &current)
		if err == nil {
			return current, nil
		}
		if !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
			return Task{}, err
		}
	}
	return Task{}, storage.ErrUnavailable
}
