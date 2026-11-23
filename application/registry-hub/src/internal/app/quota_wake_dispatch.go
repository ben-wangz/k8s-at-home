package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

const quotaWakeDispatchPrefix = "background/v1/quota-wake-dispatch/"

type quotaWakeDispatch struct {
	SchemaVersion           int       `json:"schemaVersion"`
	TaskID                  string    `json:"taskID"`
	Generation              uint64    `json:"generation"`
	RoundID                 string    `json:"roundID,omitempty"`
	SessionID               string    `json:"sessionID,omitempty"`
	LatestPendingRevision   uint64    `json:"latestPendingRevision"`
	LatestConfirmedRevision uint64    `json:"latestConfirmedRevision"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

func quotaWakeDispatchKey(taskID string) string {
	return quotaWakeDispatchPrefix + inventory.ID(taskID) + ".json"
}

func (s *Service) updateQuotaWakeDispatch(ctx context.Context, task tasks.Task) error {
	key := quotaWakeDispatchKey(task.ID)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := inventory.Read[quotaWakeDispatch](ctx, s.store, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		sameTaskEpoch := err == nil && current.SchemaVersion == 1 && current.TaskID == task.ID &&
			current.Generation == task.Generation && current.RoundID == task.RoundID && current.SessionID == task.ProtectionSessionID
		if err == nil && current.SchemaVersion == 1 && current.TaskID == task.ID && !sameTaskEpoch {
			if task.Generation < current.Generation || task.Generation == current.Generation && task.QuotaWakeRevision <= current.LatestPendingRevision {
				return nil
			}
		}
		if sameTaskEpoch && current.LatestPendingRevision >= task.QuotaWakeRevision {
			return nil
		}
		now := time.Now().UTC()
		if !sameTaskEpoch {
			current = quotaWakeDispatch{SchemaVersion: 1, TaskID: task.ID, Generation: task.Generation, RoundID: task.RoundID, SessionID: task.ProtectionSessionID}
		}
		current.LatestPendingRevision = task.QuotaWakeRevision
		current.UpdatedAt = now
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: version == ""}
		if version != "" {
			options = storage.PutOptions{IfMatch: version}
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			stored, _, readErr := inventory.Read[quotaWakeDispatch](ctx, s.store, key)
			if readErr == nil && sameQuotaWakeDispatch(stored, current) {
				s.signalBackgroundScan("quota_wake_dispatch")
				return nil
			}
			if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
				return errors.Join(err, readErr)
			}
			continue
		}
		if err != nil {
			return err
		}
		s.signalBackgroundScan("quota_wake_dispatch")
		return nil
	}
	return storage.ErrUnavailable
}

func sameQuotaWakeDispatch(left, right quotaWakeDispatch) bool {
	return left.SchemaVersion == right.SchemaVersion && left.TaskID == right.TaskID && left.Generation == right.Generation &&
		left.RoundID == right.RoundID && left.SessionID == right.SessionID &&
		left.LatestPendingRevision == right.LatestPendingRevision && left.LatestConfirmedRevision == right.LatestConfirmedRevision
}

func (s *Service) reconcileQuotaWakeDispatch(ctx context.Context, key string) error {
	current, _, err := inventory.Read[quotaWakeDispatch](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if current.SchemaVersion != 1 || current.TaskID == "" || current.LatestPendingRevision == 0 {
		return storage.ErrCorruptObject
	}
	if current.LatestPendingRevision <= current.LatestConfirmedRevision {
		return nil
	}
	task, err := s.tasks.Get(ctx, current.TaskID)
	if errors.Is(err, storage.ErrNotFound) {
		return s.confirmQuotaWakeDispatch(ctx, current, current.LatestPendingRevision)
	}
	if err != nil {
		return err
	}
	if task.Generation != current.Generation || task.RoundID != current.RoundID || task.ProtectionSessionID != current.SessionID {
		return s.updateQuotaWakeDispatch(ctx, task)
	}
	if task.QuotaWakeRevision > current.LatestPendingRevision {
		return s.updateQuotaWakeDispatch(ctx, task)
	}
	if task.QuotaWakeRevision <= current.LatestConfirmedRevision {
		return nil
	}
	work := quotaWakeWork{TaskID: task.ID, Generation: task.Generation, RoundID: task.RoundID, Revision: task.QuotaWakeRevision, SessionID: task.ProtectionSessionID}
	payload, err := json.Marshal(work)
	if err != nil {
		return err
	}
	job := workqueue.Job{Payload: payload}
	_, done, err := s.quotaWakeStep(ctx, &job)
	if err != nil {
		return err
	}
	if !done {
		return storage.ErrConditionFailed
	}
	return s.confirmQuotaWakeDispatch(ctx, current, task.QuotaWakeRevision)
}

func (s *Service) confirmQuotaWakeDispatch(ctx context.Context, expected quotaWakeDispatch, revision uint64) error {
	key := quotaWakeDispatchKey(expected.TaskID)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := inventory.Read[quotaWakeDispatch](ctx, s.store, key)
		if err != nil {
			return err
		}
		if current.Generation != expected.Generation || current.RoundID != expected.RoundID || current.SessionID != expected.SessionID {
			return nil
		}
		if revision > current.LatestPendingRevision {
			return fmt.Errorf("quota wake revision %d exceeds latest pending revision %d: %w", revision, current.LatestPendingRevision, storage.ErrConditionFailed)
		}
		if current.LatestConfirmedRevision >= revision {
			return nil
		}
		current.LatestConfirmedRevision = revision
		current.UpdatedAt = time.Now().UTC()
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version})
		if errors.Is(err, storage.ErrConditionFailed) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			stored, _, readErr := inventory.Read[quotaWakeDispatch](ctx, s.store, key)
			if readErr == nil && stored.Generation == expected.Generation && stored.RoundID == expected.RoundID &&
				stored.SessionID == expected.SessionID && stored.LatestConfirmedRevision >= revision {
				return nil
			}
			if readErr != nil {
				return errors.Join(err, readErr)
			}
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}
