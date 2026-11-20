package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
)

func (m *Manager) IsCurrentInFlight(ctx context.Context, task Task) (bool, error) {
	current, err := m.getInFlight(ctx, inFlightKey(task.Authority, task.Repository, task.Reference))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return current.ID == task.ID, nil
}

func (m *Manager) readInFlightSnapshot(ctx context.Context, authority, repository, reference string) (Task, string, bool, error) {
	key := "tasks/inflight/" + inFlightKey(authority, repository, reference)
	for attempt := 0; attempt < 16; attempt++ {
		object, err := m.store.Get(ctx, key)
		if err != nil {
			return Task{}, "", false, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil || len(data) > 1<<20 {
			return Task{}, "", false, errors.Join(readErr, closeErr, errors.New("in-flight task index could not be read safely"))
		}
		var index struct {
			SchemaVersion int    `json:"schemaVersion"`
			TaskID        string `json:"taskID"`
		}
		if err := json.Unmarshal(data, &index); err != nil || !validID(index.TaskID) {
			return Task{}, "", false, errors.New("in-flight task index is invalid")
		}
		if index.SchemaVersion != inFlightSchemaCurrent {
			if _, err := m.getInFlight(ctx, inFlightKey(authority, repository, reference)); err != nil {
				return Task{}, "", false, err
			}
			continue
		}
		task, err := m.Get(ctx, index.TaskID)
		if err != nil {
			return Task{}, "", false, err
		}
		return task, object.Version, true, nil
	}
	return Task{}, "", false, storage.ErrUnavailable
}

func (m *Manager) inFlightMatches(ctx context.Context, key, taskID string) bool {
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return false
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return false
	}
	var index struct {
		TaskID string `json:"taskID"`
	}
	return json.Unmarshal(data, &index) == nil && index.TaskID == taskID
}
