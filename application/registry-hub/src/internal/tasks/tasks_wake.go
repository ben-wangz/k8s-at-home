package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"strings"
	"time"

	"registry-hub/internal/storage"
)

const wakeRequestSchema = 1

type wakeRequest struct {
	SchemaVersion int       `json:"schemaVersion"`
	TaskID        string    `json:"taskID"`
	RequestedAt   time.Time `json:"requestedAt"`
	Generation    uint64    `json:"generation,omitempty"`
	RoundID       string    `json:"roundID,omitempty"`
}

func (m *Manager) ensureWakeRequest(ctx context.Context, taskID string) error {
	if !validID(taskID) {
		return errors.New("wake request task ID is invalid")
	}
	task, err := m.Get(ctx, taskID)
	if err != nil {
		return err
	}
	return m.ensureTaskWakeRequest(ctx, task)
}

func (m *Manager) ensureTaskWakeRequest(ctx context.Context, task Task) error {
	taskID := task.ID
	record := wakeRequest{SchemaVersion: wakeRequestSchema, TaskID: taskID, RequestedAt: time.Now().UTC(), Generation: task.Generation, RoundID: task.RoundID}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		current, err := m.Get(ctx, taskID)
		if err != nil {
			return err
		}
		if current.Generation != task.Generation || current.RoundID != task.RoundID || current.CancelRequested != task.CancelRequested || current.Status != task.Status {
			return storage.ErrConditionFailed
		}
		options := storage.PutOptions{IfAbsent: true}
		object, readErr := m.store.Get(ctx, wakeRequestKey(taskID))
		if readErr == nil {
			var existing wakeRequest
			if err := decodeRecord(object, 1<<20, &existing); err != nil {
				return err
			}
			if existing.SchemaVersion != wakeRequestSchema || existing.TaskID != taskID || existing.RequestedAt.IsZero() {
				return storage.ErrCorruptObject
			}
			if existing.Generation == record.Generation && existing.RoundID == record.RoundID && !existing.RequestedAt.Before(record.RequestedAt) {
				return nil
			}
			options = storage.PutOptions{IfMatch: object.Version}
		} else if !errors.Is(readErr, storage.ErrNotFound) {
			return readErr
		}
		if err := inventory.ValidateWriteGuard(ctx); err != nil {
			return err
		}
		_, err = m.store.Put(ctx, wakeRequestKey(taskID), bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}

func (m *Manager) clearWakeRequest(ctx context.Context, taskID string) error {
	if !validID(taskID) {
		return errors.New("wake request task ID is invalid")
	}
	object, err := m.store.Get(ctx, wakeRequestKey(taskID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var request wakeRequest
	if err := decodeRecord(object, 1<<20, &request); err != nil {
		return err
	}
	current, err := m.Get(ctx, taskID)
	if err == nil && (current.Status == Running || isWaiting(current.Status) || request.Generation == current.Generation && request.RoundID == current.RoundID && request.RequestedAt.After(current.UpdatedAt)) {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return m.clearObservedWakeRequest(ctx, taskID, object.Version)
}

func (m *Manager) reconcileWakeRequests(ctx context.Context) error {
	token := ""
	for {
		entries, next, err := storage.Scan(ctx, m.store, "tasks/wake/", token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			taskID := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "tasks/wake/"), ".json")
			if !validID(taskID) {
				return errors.New("wake request key is invalid")
			}
			object, err := m.store.Get(ctx, entry.Key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			var request wakeRequest
			if err := decodeRecord(object, 1<<20, &request); err != nil {
				return err
			}
			task, err := m.Get(ctx, taskID)
			if errors.Is(err, storage.ErrNotFound) {
				if err := m.clearObservedWakeRequest(ctx, taskID, object.Version); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if request.SchemaVersion != wakeRequestSchema || request.TaskID != taskID || request.RoundID != task.RoundID || request.Generation != task.Generation {
				if err := m.clearObservedWakeRequest(ctx, taskID, object.Version); err != nil {
					return err
				}
				continue
			}
			if task.CancelRequested && task.Status != Cancelled && task.Status != Failed && task.Status != Solidified {
				continue
			}
			switch task.Status {
			case WaitingCache, WaitingDeps, WaitingQuota, VerifiedLocal:
				if _, err := m.WakeMatching(ctx, task); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
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

func (m *Manager) clearObservedWakeRequest(ctx context.Context, taskID, version string) error {
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	err := m.store.DeleteIfMatch(ctx, wakeRequestKey(taskID), version)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		return nil
	}
	return err
}

func wakeRequestKey(taskID string) string {
	return "tasks/wake/" + taskID + ".json"
}
