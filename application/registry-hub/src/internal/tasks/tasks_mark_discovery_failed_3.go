package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"slices"
	"time"
)

func (m *Manager) MarkDiscoveryFailed(ctx context.Context, task *Task, class string, cause error) {
	task.Status = Failed
	task.Phase = "discovery"
	task.Retryable = false
	task.ErrorClass = class
	task.ErrorMessage = safeMessage(cause)
	task.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, task); err == nil {
		m.finishInFlight(ctx, task)
	}
}

func (m *Manager) Get(ctx context.Context, id string) (Task, error) {
	if !validID(id) {
		return Task{}, storage.ErrNotFound
	}
	return m.readTask(ctx, taskKey(id))
}

func (m *Manager) List(ctx context.Context, prefix, token string, limit int) ([]Task, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := m.store.List(ctx, "tasks/state/"+prefix, token, limit)
	if err != nil {
		return nil, "", err
	}
	tasks := make([]Task, 0, len(entries))
	for _, entry := range entries {
		task, readErr := m.readTask(ctx, entry.Key)
		if readErr != nil {
			return nil, "", readErr
		}
		tasks = append(tasks, task)
	}
	return tasks, next, nil
}

func (m *Manager) Active(ctx context.Context) ([]Task, error) {
	return m.listActive(ctx)
}

func (m *Manager) SetProtectionState(ctx context.Context, id, scopeID, sessionID string, reservedBytes int64, protected bool) error {
	task, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if protected {
		if scopeID == "" || !validID(sessionID) || reservedBytes < 0 {
			return errors.New("task protection state is invalid")
		}
		task.ScopeID = scopeID
		task.SessionIDs = []string{sessionID}
		task.RequestedBytes = reservedBytes
		task.ReservedBytes = reservedBytes
		task.GCProtected = true
	} else {
		task.ScopeID = ""
		task.SessionIDs = nil
		task.RequestedBytes = 0
		task.ReservedBytes = 0
		task.GCProtected = false
		task.RecoveryReason = ""
	}
	task.UpdatedAt = time.Now().UTC()
	return m.save(ctx, &task)
}

func (m *Manager) AddProtectionSessionState(ctx context.Context, id, sessionID string, requestedBytes, reservedBytes int64) error {
	if !validID(sessionID) || requestedBytes < 0 || reservedBytes < 0 {
		return errors.New("task protection session state is invalid")
	}
	task, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	for _, existing := range task.SessionIDs {
		if existing == sessionID {
			task.RequestedBytes = requestedBytes
			task.ReservedBytes = reservedBytes
			task.GCProtected = true
			task.UpdatedAt = time.Now().UTC()
			return m.save(ctx, &task)
		}
	}
	if task.ScopeID == "" {
		return errors.New("task protection scope is missing")
	}
	task.SessionIDs = append(task.SessionIDs, sessionID)
	task.RequestedBytes = requestedBytes
	task.ReservedBytes = reservedBytes
	task.GCProtected = true
	task.UpdatedAt = time.Now().UTC()
	return m.save(ctx, &task)
}

func (m *Manager) ReconcileProtectionState(ctx context.Context, id, scopeID string, sessionIDs []string, requestedBytes, reservedBytes int64) error {
	if len(sessionIDs) > 4096 || requestedBytes < 0 || reservedBytes < 0 || len(sessionIDs) == 0 && (requestedBytes != 0 || reservedBytes != 0) {
		return errors.New("reconciled task protection state is invalid")
	}
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		if !validID(sessionID) {
			return errors.New("reconciled task protection contains an invalid session ID")
		}
		if _, exists := seen[sessionID]; exists {
			return errors.New("reconciled task protection contains duplicate session IDs")
		}
		seen[sessionID] = struct{}{}
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		protectionSessionID := ""
		protectionOwnerID := ""
		if len(sessionIDs) > 0 && task.Kind == BlobKind {
			protectionSessionID = sessionIDs[0]
			protectionOwnerID = task.ID
		}
		protectionMetadataMatches := task.Kind != BlobKind ||
			task.ProtectionSessionID == protectionSessionID && task.ProtectionOwnerID == protectionOwnerID
		if task.ScopeID == scopeID && slices.Equal(task.SessionIDs, sessionIDs) &&
			task.RequestedBytes == requestedBytes && task.ReservedBytes == reservedBytes &&
			task.GCProtected == (len(sessionIDs) > 0) && task.RecoveryReason == "" &&
			protectionMetadataMatches {
			return nil
		}
		task.ScopeID = scopeID
		task.SessionIDs = append([]string(nil), sessionIDs...)
		task.RequestedBytes = requestedBytes
		task.ReservedBytes = reservedBytes
		task.GCProtected = len(sessionIDs) > 0
		task.RecoveryReason = ""
		if task.Kind == BlobKind {
			if len(sessionIDs) > 0 {
				task.ProtectionSessionID = protectionSessionID
				task.ProtectionOwnerID = protectionOwnerID
			} else {
				task.ProtectionSessionID = ""
				task.ProtectionOwnerID = ""
			}
		}
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (m *Manager) SetLegacyProtectionBytes(ctx context.Context, id string, reservedBytes int64) error {
	if reservedBytes < 1 {
		return errors.New("legacy protection reservation must be positive")
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.LegacyProtectionBytes != 0 {
			if task.LegacyProtectionBytes != reservedBytes {
				return errors.New("legacy protection reservation changed")
			}
			return nil
		}
		task.LegacyProtectionBytes = reservedBytes
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}
