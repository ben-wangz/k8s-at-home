package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func NewManager(store storage.Store, workers, pendingMax int, processor Processor) (*Manager, error) {
	owner, err := randomID()
	if err != nil {
		return nil, err
	}
	return &Manager{
		store: store, leases: lease.NewManager(store), processor: processor,
		workers: workers, pendingMax: pendingMax, maxWaitingRoots: pendingMax, idleTimeout: 24 * time.Hour,
		active: make(map[string]struct{}), cancels: make(map[string]context.CancelFunc), owner: owner,
	}, nil
}

func (m *Manager) SetMaxWaitingRoots(limit int) {
	if limit > 0 {
		m.maxWaitingRoots = limit
	}
}

func (m *Manager) SetIdleTimeout(timeout time.Duration) {
	if timeout > 0 {
		m.idleTimeout = timeout
	}
}

func (m *Manager) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	for worker := 0; worker < m.workers; worker++ {
		m.wg.Add(1)
		go m.run(ctx, worker)
	}
}

func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

func (m *Manager) Enqueue(ctx context.Context, authority, repository, reference string) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	requestKey := inFlightKey(authority, repository, reference)
	if current, err := m.getInFlight(ctx, requestKey); err == nil {
		if isActive(current.Status) {
			if current.Phase != "discovery" || time.Since(current.CreatedAt) < 5*time.Minute {
				return current, false, nil
			}
			current.Status = Failed
			current.ErrorClass = "interrupted_discovery"
			current.ErrorMessage = "request discovery did not complete"
			current.UpdatedAt = time.Now().UTC()
			if saveErr := m.save(ctx, &current); saveErr != nil {
				return Task{}, false, saveErr
			}
		}
		_ = m.deleteInFlight(ctx, requestKey)
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Task{}, false, err
	}
	activeTasks, err := m.listActive(ctx)
	if err != nil {
		return Task{}, false, err
	}
	if err := m.checkRootDemandCapacity(activeTasks, ""); err != nil {
		return Task{}, false, err
	}
	id, err := randomID()
	if err != nil {
		return Task{}, false, err
	}
	now := time.Now().UTC()
	task := Task{
		SchemaVersion: taskSchemaCurrent, ID: id, Kind: RootKind, Authority: authority, Repository: repository,
		ProtocolVersion: 2, RoundID: id,
		Discovery: "manifest", Reference: reference, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(m.idleTimeout),
		LastProgressAt: now,
		Status:         Pending, Phase: "discovery", Graph: make(map[string]GraphEntry), Checkpoints: make(map[string]string),
	}
	if err := m.create(ctx, &task); err != nil {
		return Task{}, false, err
	}
	indexData, _ := json.Marshal(struct {
		SchemaVersion int    `json:"schemaVersion"`
		TaskID        string `json:"taskID"`
	}{SchemaVersion: inFlightSchemaCurrent, TaskID: id})
	indexKey := "tasks/inflight/" + requestKey
	_, err = m.store.Put(ctx, indexKey, bytes.NewReader(indexData), int64(len(indexData)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrUnknownCommit) {
		if m.inFlightMatches(ctx, indexKey, id) {
			return task, true, nil
		}
		return Task{}, false, err
	}
	if err != nil {
		return Task{}, false, errors.Join(err, m.discardCreatedTask(ctx, task))
	}
	return task, true, nil
}

func (m *Manager) SetDiscovered(ctx context.Context, task *Task, digest, mediaType string, generation ...uint64) error {
	if digest == "" || mediaType == "" {
		return errors.New("discovered artifact requires digest and media type")
	}
	progressed := task.RootDigest != digest || task.MediaType != mediaType
	task.RootDigest = digest
	task.MediaType = mediaType
	if len(generation) > 0 {
		progressed = progressed || task.Generation != generation[0]
		task.Generation = generation[0]
	}
	task.Phase = "solidification"
	task.Status = Pending
	task.UpdatedAt = time.Now().UTC()
	if progressed {
		task.LastProgressAt = task.UpdatedAt
		task.ExpiresAt = task.UpdatedAt.Add(m.idleTimeout)
	}
	return m.save(ctx, task)
}

func (m *Manager) Wake(ctx context.Context, id string) (Task, error) {
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if task.CancelRequested {
			return Task{}, errors.New("task is being cancelled")
		}
		if task.Status == Running {
			if err := m.ensureWakeRequest(ctx, id); err != nil {
				return Task{}, err
			}
			return task, nil
		}
		if task.Status == Pending {
			if err := m.clearWakeRequest(ctx, id); err != nil {
				return Task{}, err
			}
			return task, nil
		}
		if !isWaiting(task.Status) {
			return Task{}, errors.New("task is not waiting or running")
		}
		task.Status = Pending
		task.WaitReason = ""
		task.QuotaWaitBytes = 0
		task.QuotaReplaceableBytes = 0
		task.QuotaReplaceableSlots = 0
		task.NextAttemptAt = time.Time{}
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := m.Get(ctx, id)
			if readErr == nil && latest.Status == Pending {
				if clearErr := m.clearWakeRequest(ctx, id); clearErr != nil {
					return Task{}, clearErr
				}
				return latest, nil
			}
			continue
		} else if err != nil {
			return Task{}, err
		}
		if err := m.clearWakeRequest(ctx, id); err != nil {
			return Task{}, err
		}
		return task, nil
	}
	return Task{}, storage.ErrUnavailable
}
