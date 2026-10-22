package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) EnqueueResolved(ctx context.Context, authority, repository, reference, digest, mediaType string, generation uint64) (Task, bool, error) {
	return m.enqueueResolved(ctx, "", authority, repository, reference, digest, mediaType, generation)
}

func (m *Manager) ResolveDiscovery(ctx context.Context, discoveryTaskID, authority, repository, reference, digest, mediaType string, generation uint64) (Task, bool, error) {
	if !validID(discoveryTaskID) {
		return Task{}, false, errors.New("discovery task identity is invalid")
	}
	return m.enqueueResolved(ctx, discoveryTaskID, authority, repository, reference, digest, mediaType, generation)
}

func (m *Manager) enqueueResolved(ctx context.Context, expectedTaskID, authority, repository, reference, digest, mediaType string, generation uint64) (Task, bool, error) {
	if authority == "" || repository == "" || reference == "" || digest == "" || mediaType == "" {
		return Task{}, false, errors.New("resolved manifest identity is incomplete")
	}
	m.mu.Lock()
	var replaced Task
	for attempt := 0; attempt < 16; attempt++ {
		current, indexVersion, exists, err := m.readInFlightSnapshot(ctx, authority, repository, reference)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			m.mu.Unlock()
			return Task{}, false, err
		}
		if expectedTaskID != "" && (!exists || current.ID != expectedTaskID) {
			m.mu.Unlock()
			return Task{}, false, ErrCancelled
		}
		if expectedTaskID != "" && (current.Authority != authority || current.Repository != repository || current.Reference != reference) {
			m.mu.Unlock()
			return Task{}, false, errors.New("discovery task identity does not match the manifest reference")
		}
		if exists && isActive(current.Status) {
			if expectedTaskID != "" && current.CancelRequested {
				m.mu.Unlock()
				return Task{}, false, ErrCancelled
			}
			if current.RootDigest == digest && current.MediaType == mediaType && current.Generation == generation {
				m.mu.Unlock()
				return current, false, nil
			}
			if current.RootDigest == "" && current.Phase == "discovery" {
				if err := m.SetDiscovered(ctx, &current, digest, mediaType, generation); err != nil {
					if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
						latest, readErr := m.Get(ctx, current.ID)
						if readErr == nil && (latest.CancelRequested || !isActive(latest.Status)) {
							m.mu.Unlock()
							return Task{}, false, ErrCancelled
						}
						if readErr == nil && latest.RootDigest == digest && latest.MediaType == mediaType && latest.Generation == generation && isActive(latest.Status) {
							m.mu.Unlock()
							return latest, false, nil
						}
						if readErr != nil {
							err = errors.Join(err, readErr)
						}
					}
					m.mu.Unlock()
					return Task{}, false, err
				}
				m.mu.Unlock()
				return current, false, nil
			}
			replaced = current
		} else if expectedTaskID != "" {
			m.mu.Unlock()
			return Task{}, false, ErrCancelled
		}
		active, err := m.listActive(ctx)
		if err != nil {
			m.mu.Unlock()
			return Task{}, false, err
		}
		activeCount := runnableTaskCount(active)
		if replaced.ID != "" {
			if isRunnableTask(replaced) {
				activeCount--
			}
		}
		if activeCount >= m.pendingMax {
			m.mu.Unlock()
			return Task{}, false, errors.New("solidification queue is full")
		}
		if err := m.checkRootDemandCapacity(active, replaced.ID); err != nil {
			m.mu.Unlock()
			return Task{}, false, err
		}
		id, err := randomID()
		if err != nil {
			m.mu.Unlock()
			return Task{}, false, err
		}
		now := time.Now().UTC()
		task := Task{
			SchemaVersion: taskSchemaCurrent, ProtocolVersion: 2, ID: id, Kind: RootKind, Authority: authority, Repository: repository, RoundID: id,
			Discovery: "manifest", Reference: reference, RootDigest: digest, MediaType: mediaType, Generation: generation,
			CreatedAt: now, UpdatedAt: now, LastProgressAt: now, ExpiresAt: now.Add(m.idleTimeout),
			Status: Pending, Phase: "solidification", Graph: make(map[string]GraphEntry), Checkpoints: make(map[string]string),
		}
		if err := m.create(ctx, &task); err != nil {
			m.mu.Unlock()
			return Task{}, false, err
		}
		indexData, _ := json.Marshal(struct {
			SchemaVersion int    `json:"schemaVersion"`
			TaskID        string `json:"taskID"`
		}{SchemaVersion: inFlightSchemaCurrent, TaskID: task.ID})
		key := "tasks/inflight/" + inFlightKey(authority, repository, reference)
		options := storage.PutOptions{IfAbsent: true}
		if indexVersion != "" {
			options = storage.PutOptions{IfMatch: indexVersion}
		}
		_, writeErr := m.store.Put(ctx, key, bytes.NewReader(indexData), int64(len(indexData)), options)
		if writeErr == nil || errors.Is(writeErr, storage.ErrUnknownCommit) && m.inFlightMatches(ctx, key, task.ID) {
			m.mu.Unlock()
			if replaced.ID != "" {
				_, _ = m.Cancel(ctx, replaced.ID)
			}
			return task, true, nil
		}
		if errors.Is(writeErr, storage.ErrUnknownCommit) {
			m.mu.Unlock()
			return Task{}, false, writeErr
		}
		cleanupErr := m.discardCreatedTask(ctx, task)
		if cleanupErr != nil {
			m.mu.Unlock()
			return Task{}, false, errors.Join(writeErr, cleanupErr)
		}
		if errors.Is(writeErr, storage.ErrConditionFailed) || errors.Is(writeErr, storage.ErrAlreadyExists) {
			replaced = Task{}
			continue
		}
		m.mu.Unlock()
		return Task{}, false, writeErr
	}
	m.mu.Unlock()
	return Task{}, false, storage.ErrUnavailable
}

func runnableTaskCount(items []Task) int {
	count := 0
	for _, item := range items {
		if isRunnableTask(item) {
			count++
		}
	}
	return count
}

func isRunnableTask(task Task) bool {
	return task.Phase == "solidification" && (task.Status == Pending || task.Status == Running || task.Status == RetryWait)
}

func (m *Manager) checkRootDemandCapacity(active []Task, exceptID string) error {
	limit := m.maxWaitingRoots
	if limit < 1 {
		limit = m.pendingMax
	}
	count := 0
	for _, task := range active {
		if task.Kind == RootKind && task.ID != exceptID {
			count++
		}
	}
	if count >= limit {
		return errors.New("solidification root demand quota is full")
	}
	return nil
}

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
