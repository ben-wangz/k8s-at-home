package tasks

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) create(ctx context.Context, task *Task) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	entry, err := m.store.Put(ctx, taskKey(task.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		return err
	}
	task.version = entry.Version
	return nil
}

func (m *Manager) save(ctx context.Context, task *Task) error {
	if task.version == "" {
		return errors.New("task state has no storage version")
	}
	if task.SchemaVersion != taskSchemaCurrent {
		return errors.New("task state has an unsupported schema")
	}
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	entry, err := m.store.Put(ctx, taskKey(task.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: task.version})
	if err != nil {
		return err
	}
	task.version = entry.Version
	return nil
}

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
			task.version = object.Version
		case taskSchemaPrevious:
			task.SchemaVersion = taskSchemaCurrent
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
		if task.Checkpoints == nil {
			task.Checkpoints = make(map[string]string)
		}
		if task.Graph == nil {
			task.Graph = make(map[string]GraphEntry)
		}
		return task, nil
	}
	return Task{}, storage.ErrUnavailable
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
			if task.Status == Pending || task.Status == Running || task.Status == RetryWait {
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

func (m *Manager) deleteInFlight(ctx context.Context, requestKey string) error {
	key := "tasks/inflight/" + requestKey
	entry, err := m.store.Head(ctx, key)
	if err != nil {
		return err
	}
	return m.store.DeleteIfMatch(ctx, key, entry.Version)
}

func (m *Manager) finishInFlight(ctx context.Context, task *Task) {
	requestKey := inFlightKey(task.Authority, task.Repository, task.Reference)
	key := "tasks/inflight/" + requestKey
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	if err != nil {
		return
	}
	var index struct {
		TaskID string `json:"taskID"`
	}
	if json.Unmarshal(data, &index) != nil || index.TaskID != task.ID {
		return
	}
	_ = m.store.DeleteIfMatch(ctx, key, object.Version)
}

func taskKey(id string) string { return "tasks/state/" + id + ".json" }

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func inFlightKey(authority, repository, reference string) string {
	hash := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + reference))
	return hex.EncodeToString(hash[:])
}

func retryDelay(retry int) time.Duration {
	if retry > 9 {
		retry = 9
	}
	if retry < 1 {
		retry = 1
	}
	delay := time.Second << (retry - 1)
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func safeMessage(error) string { return "task operation failed; inspect classified logs" }
