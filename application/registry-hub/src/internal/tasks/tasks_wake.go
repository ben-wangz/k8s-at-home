package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/storage"
)

const wakeRequestSchema = 1

type wakeRequest struct {
	SchemaVersion int       `json:"schemaVersion"`
	TaskID        string    `json:"taskID"`
	RequestedAt   time.Time `json:"requestedAt"`
}

func (m *Manager) ensureWakeRequest(ctx context.Context, taskID string) error {
	if !validID(taskID) {
		return errors.New("wake request task ID is invalid")
	}
	record := wakeRequest{SchemaVersion: wakeRequestSchema, TaskID: taskID, RequestedAt: time.Now().UTC()}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = m.store.Put(ctx, wakeRequestKey(taskID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	object, readErr := m.store.Get(ctx, wakeRequestKey(taskID))
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	var existing wakeRequest
	if decodeErr := decodeRecord(object, 1<<20, &existing); decodeErr != nil {
		return decodeErr
	}
	if existing.SchemaVersion != wakeRequestSchema || existing.TaskID != taskID || existing.RequestedAt.IsZero() {
		return errors.New("wake request record is invalid")
	}
	return nil
}

func (m *Manager) clearWakeRequest(ctx context.Context, taskID string) error {
	if !validID(taskID) {
		return errors.New("wake request task ID is invalid")
	}
	key := wakeRequestKey(taskID)
	for attempt := 0; attempt < 16; attempt++ {
		entry, err := m.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = m.store.DeleteIfMatch(ctx, key, entry.Version)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}

func (m *Manager) reconcileWakeRequests(ctx context.Context) error {
	token := ""
	for {
		entries, next, err := m.store.List(ctx, "tasks/wake/", token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			taskID := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "tasks/wake/"), ".json")
			if !validID(taskID) {
				return errors.New("wake request key is invalid")
			}
			task, err := m.Get(ctx, taskID)
			if errors.Is(err, storage.ErrNotFound) {
				if err := m.clearWakeRequest(ctx, taskID); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if task.CancelRequested && task.Status != Cancelled && task.Status != Failed && task.Status != Solidified {
				continue
			}
			switch task.Status {
			case WaitingCache, WaitingDeps, WaitingQuota, VerifiedLocal:
				if _, err := m.Wake(ctx, task.ID); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
					return err
				}
			case Pending:
				if err := m.clearWakeRequest(ctx, task.ID); err != nil {
					return err
				}
			case Running:
			default:
				if err := m.clearWakeRequest(ctx, task.ID); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func wakeRequestKey(taskID string) string {
	return "tasks/wake/" + taskID + ".json"
}
