package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
)

func (m *Manager) readTask(ctx context.Context, key string) (Task, error) {
	for attempt := 0; attempt < 16; attempt++ {
		object, err := m.store.Get(ctx, key)
		if err != nil {
			return Task{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 16<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return Task{}, readErr
		}
		if closeErr != nil {
			return Task{}, closeErr
		}
		if len(data) > 16<<20 {
			return Task{}, errors.New("task state exceeds size limit")
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			return Task{}, fmt.Errorf("decode task state: %w", err)
		}
		if !validID(task.ID) || taskKey(task.ID) != key {
			return Task{}, errors.New("task state has an unsupported schema or identity")
		}
		switch task.SchemaVersion {
		case taskSchemaCurrent:
			if task.Kind == "" {
				task.Kind = RootKind
			}
			if task.Kind != RootKind && task.Kind != BlobKind {
				return Task{}, errors.New("task state has an unsupported kind")
			}
			task.version = object.Version
		case taskSchemaPrevious, taskSchemaLegacy:
			task.SchemaVersion = taskSchemaCurrent
			if task.Kind == "" {
				task.Kind = RootKind
			}
			if task.Kind != RootKind && task.Kind != BlobKind {
				return Task{}, errors.New("task state has an unsupported kind")
			}
			if task.LastProgressAt.IsZero() {
				task.LastProgressAt = task.UpdatedAt
			}
			updated, marshalErr := json.Marshal(task)
			if marshalErr != nil {
				return Task{}, marshalErr
			}
			entry, putErr := m.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return Task{}, putErr
			}
			task.version = entry.Version
		default:
			return Task{}, errors.New("task state has an unsupported schema or identity")
		}
		if task.ExpiresAt.IsZero() {
			progressAt := task.LastProgressAt
			if progressAt.IsZero() {
				progressAt = task.UpdatedAt
			}
			if progressAt.IsZero() {
				progressAt = task.CreatedAt
			}
			if !progressAt.IsZero() {
				task.ExpiresAt = progressAt.Add(m.idleTimeout)
			}
		}
		if task.Checkpoints == nil {
			task.Checkpoints = make(map[string]string)
		}
		if task.Graph == nil {
			task.Graph = make(map[string]GraphEntry)
		}
		if err := validateTaskProtectionState(&task); err != nil {
			return Task{}, err
		}
		return task, nil
	}
	return Task{}, storage.ErrUnavailable
}

func validateTaskProtectionState(task *Task) error {
	if task.RequestedBytes < 0 || task.ReservedBytes < 0 || task.QuotaWaitBytes < 0 || task.QuotaReplaceableBytes < 0 || task.QuotaReplaceableSlots < 0 || task.QuotaReplaceableSlots > 4096 || task.QuotaWaitReason != "" && !validWaitReason(task.QuotaWaitReason) || len(task.SessionIDs) > 4096 {
		return errors.New("task protection state is invalid")
	}
	seen := make(map[string]struct{}, len(task.SessionIDs))
	for _, id := range task.SessionIDs {
		if !validID(id) {
			return errors.New("task protection state contains an invalid session ID")
		}
		if _, exists := seen[id]; exists {
			return errors.New("task protection state contains a duplicate session ID")
		}
		seen[id] = struct{}{}
	}
	if task.GCProtected && (task.ScopeID == "" || len(task.SessionIDs) == 0) {
		return errors.New("protected task is missing its scope or session IDs")
	}
	return nil
}

func (m *Manager) listActive(ctx context.Context) ([]Task, error) {
	var result []Task
	token := ""
	for {
		entries, next, err := m.store.List(ctx, "tasks/state/", token, 1000)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			task, readErr := m.readTask(ctx, entry.Key)
			if readErr != nil {
				return nil, readErr
			}
			if isActive(task.Status) {
				result = append(result, task)
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return result, nil
}

func (m *Manager) getInFlight(ctx context.Context, requestKey string) (Task, error) {
	key := "tasks/inflight/" + requestKey
	for attempt := 0; attempt < 16; attempt++ {
		object, err := m.store.Get(ctx, key)
		if err != nil {
			return Task{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return Task{}, readErr
		}
		if closeErr != nil {
			return Task{}, closeErr
		}
		if len(data) > 1<<20 {
			return Task{}, errors.New("in-flight task index exceeds size limit")
		}
		var index struct {
			SchemaVersion int    `json:"schemaVersion"`
			TaskID        string `json:"taskID"`
		}
		if err := json.Unmarshal(data, &index); err != nil || !validID(index.TaskID) {
			return Task{}, errors.New("in-flight task index is invalid")
		}
		switch index.SchemaVersion {
		case inFlightSchemaCurrent:
			return m.Get(ctx, index.TaskID)
		case inFlightSchemaPrevious:
			index.SchemaVersion = inFlightSchemaCurrent
			updated, marshalErr := json.Marshal(index)
			if marshalErr != nil {
				return Task{}, marshalErr
			}
			if _, putErr := m.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version}); errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			} else if putErr != nil {
				return Task{}, putErr
			}
		default:
			return Task{}, errors.New("in-flight task index is invalid")
		}
	}
	return Task{}, storage.ErrUnavailable
}
