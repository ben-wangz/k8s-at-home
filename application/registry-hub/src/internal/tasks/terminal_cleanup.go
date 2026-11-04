package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

// The terminal task is the durable outbox when enqueueing its cleanup fails.
func (m *Manager) PrepareTerminalCleanup(ctx context.Context, id string) (Task, error) {
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if task.Status != Failed && task.Status != Cancelled && task.Status != Solidified || task.TerminalCleanupComplete || task.TerminalCleanupID != "" {
			return task, nil
		}
		task.TerminalCleanupID, err = randomID()
		if err != nil {
			return Task{}, err
		}
		err = m.save(ctx, &task)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return task, err
	}
	return Task{}, storage.ErrUnavailable
}
