package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"sync"
	"time"
)

type State string

type GraphEntry struct {
	Digest       string            `json:"digest"`
	MediaType    string            `json:"mediaType"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     json.RawMessage   `json:"platform,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
}

var ErrCancelled = errors.New("task was cancelled")
var ErrRootPublishing = errors.New("task root publication has started")

const (
	taskSchemaPrevious     = 1
	taskSchemaCurrent      = 2
	inFlightSchemaPrevious = 1
	inFlightSchemaCurrent  = 2

	Pending       State = "pending"
	Running       State = "running"
	RetryWait     State = "retry_wait"
	Solidified    State = "solidified"
	Failed        State = "failed"
	Cancelled     State = "cancelled"
	RepairPending State = "repair_pending"
)

type Task struct {
	SchemaVersion   int                   `json:"schemaVersion"`
	ID              string                `json:"id"`
	Authority       string                `json:"authority"`
	Repository      string                `json:"repository"`
	RootDigest      string                `json:"rootDigest,omitempty"`
	MediaType       string                `json:"mediaType,omitempty"`
	Discovery       string                `json:"discovery"`
	Reference       string                `json:"reference"`
	Generation      uint64                `json:"generation"`
	GraphVersion    uint64                `json:"graphVersion"`
	Graph           map[string]GraphEntry `json:"graph,omitempty"`
	Checkpoints     map[string]string     `json:"checkpoints,omitempty"`
	RetryCount      int                   `json:"retryCount"`
	NextAttemptAt   time.Time             `json:"nextAttemptAt,omitempty"`
	CreatedAt       time.Time             `json:"createdAt"`
	UpdatedAt       time.Time             `json:"updatedAt"`
	Status          State                 `json:"status"`
	Phase           string                `json:"phase"`
	CancelRequested bool                  `json:"cancelRequested,omitempty"`
	RootPublishing  bool                  `json:"rootPublishing,omitempty"`
	RootPublished   bool                  `json:"rootPublished,omitempty"`
	Retryable       bool                  `json:"retryable,omitempty"`
	ErrorClass      string                `json:"errorClass,omitempty"`
	ErrorMessage    string                `json:"errorMessage,omitempty"`
	FencingToken    uint64                `json:"fencingToken,omitempty"`
	version         string
}

type Failure struct {
	Class         string
	Permanent     bool
	RepairPending bool
	Err           error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

type Processor interface {
	Process(context.Context, *Task, lease.Lease) error
}

type Manager struct {
	store      storage.Store
	leases     *lease.Manager
	processor  Processor
	workers    int
	pendingMax int
	mu         sync.Mutex
	active     map[string]struct{}
	owner      string
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

func NewManager(store storage.Store, workers, pendingMax int, processor Processor) (*Manager, error) {
	owner, err := randomID()
	if err != nil {
		return nil, err
	}
	return &Manager{
		store: store, leases: lease.NewManager(store), processor: processor,
		workers: workers, pendingMax: pendingMax, active: make(map[string]struct{}), owner: owner,
	}, nil
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
		if current.Status == Pending || current.Status == Running || current.Status == RetryWait || current.Status == RepairPending {
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
	if len(activeTasks) >= m.pendingMax {
		return Task{}, false, errors.New("solidification queue is full")
	}
	id, err := randomID()
	if err != nil {
		return Task{}, false, err
	}
	now := time.Now().UTC()
	task := Task{
		SchemaVersion: taskSchemaCurrent, ID: id, Authority: authority, Repository: repository,
		Discovery: "manifest", Reference: reference, CreatedAt: now, UpdatedAt: now,
		Status: Pending, Phase: "discovery", Graph: make(map[string]GraphEntry), Checkpoints: make(map[string]string),
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
	if err != nil {
		_ = m.store.DeleteIfMatch(ctx, taskKey(id), task.version)
		return Task{}, false, err
	}
	return task, true, nil
}

func (m *Manager) SetDiscovered(ctx context.Context, task *Task, digest, mediaType string, generation ...uint64) error {
	if digest == "" || mediaType == "" {
		return errors.New("discovered artifact requires digest and media type")
	}
	task.RootDigest = digest
	task.MediaType = mediaType
	if len(generation) > 0 {
		task.Generation = generation[0]
	}
	task.Phase = "solidification"
	task.Status = Pending
	task.UpdatedAt = time.Now().UTC()
	return m.save(ctx, task)
}

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
