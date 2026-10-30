package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) create(ctx context.Context, task *Task) error {
	if err := m.ensureRootCacheProtectionReference(ctx, task); err != nil {
		return errors.Join(err, m.cleanupUncommittedCacheProtectionReferences(ctx, task.ID))
	}
	recordTaskEvent(task)
	data, err := json.Marshal(task)
	if err != nil {
		return errors.Join(err, m.cleanupUncommittedCacheProtectionReferences(ctx, task.ID))
	}
	entry, err := m.store.Put(ctx, taskKey(task.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		current, readErr := m.Get(ctx, task.ID)
		if readErr == nil {
			if sameTaskCreationIdentity(current, *task) {
				*task = current
				return nil
			}
			return err
		}
		if errors.Is(readErr, storage.ErrNotFound) {
			return errors.Join(err, m.cleanupUncommittedCacheProtectionReferences(ctx, task.ID))
		}
		return errors.Join(err, readErr)
	}
	task.version = entry.Version
	m.runtimeProgress(task.LastProgressAt)
	return nil
}

func sameTaskCreationIdentity(current, requested Task) bool {
	return current.ID == requested.ID && current.Kind == requested.Kind && current.Authority == requested.Authority &&
		current.Repository == requested.Repository && current.Reference == requested.Reference && current.RootDigest == requested.RootDigest &&
		current.MediaType == requested.MediaType && current.Generation == requested.Generation
}

func (m *Manager) discardCreatedTask(ctx context.Context, task Task) error {
	current, err := m.Get(ctx, task.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return m.cleanupUncommittedCacheProtectionReferences(ctx, task.ID)
	}
	if err != nil {
		return err
	}
	if current.version != task.version || !sameTaskCreationIdentity(current, task) {
		return nil
	}
	if err := m.store.DeleteIfMatch(context.WithoutCancel(ctx), taskKey(task.ID), current.version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		if !errors.Is(err, storage.ErrUnknownCommit) && !errors.Is(err, storage.ErrConditionFailed) {
			return err
		}
		_, readErr := m.Get(context.WithoutCancel(ctx), task.ID)
		if errors.Is(readErr, storage.ErrNotFound) {
			return m.cleanupUncommittedCacheProtectionReferences(context.WithoutCancel(ctx), task.ID)
		}
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		return nil
	}
	return m.cleanupUncommittedCacheProtectionReferences(context.WithoutCancel(ctx), task.ID)
}

func (m *Manager) cleanupUncommittedCacheProtectionReferences(ctx context.Context, taskID string) error {
	if !validID(taskID) {
		return errors.New("cache protection task ID is invalid")
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := m.Get(cleanupCtx, taskID); err == nil {
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	records := NewRecords(m.store)
	for {
		_, done, err := records.DeleteCacheProtectionReferences(cleanupCtx, taskID, 1000)
		if err != nil || done {
			return err
		}
	}
}

func (m *Manager) save(ctx context.Context, task *Task) error {
	if task.version == "" {
		return errors.New("task state has no storage version")
	}
	if task.SchemaVersion != taskSchemaCurrent {
		return errors.New("task state has an unsupported schema")
	}
	if err := validateTaskProtectionState(task); err != nil {
		return err
	}
	if err := m.ensureRootCacheProtectionReference(ctx, task); err != nil {
		return err
	}
	recordTaskEvent(task)
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	entry, err := m.store.Put(ctx, taskKey(task.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: task.version})
	if err != nil {
		return err
	}
	task.version = entry.Version
	m.runtimeProgress(task.LastProgressAt)
	return nil
}

func (m *Manager) ensureRootCacheProtectionReference(ctx context.Context, task *Task) error {
	if task.Kind != RootKind || task.RootDigest == "" {
		return nil
	}
	return NewRecords(m.store).EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, task.RootDigest)
}
