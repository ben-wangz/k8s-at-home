package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"slices"
	"sync"
	"time"
)

type State string
type Kind string

type GraphEntry struct {
	Digest       string            `json:"digest"`
	MediaType    string            `json:"mediaType"`
	Size         int64             `json:"size"`
	ArtifactType string            `json:"artifactType,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Platform     json.RawMessage   `json:"platform,omitempty"`
	URLs         []string          `json:"urls,omitempty"`
	Data         json.RawMessage   `json:"data,omitempty"`
}

var ErrCancelled = errors.New("task was cancelled")
var ErrRootPublishing = errors.New("task root publication has started")

const (
	taskSchemaPrevious     = 1
	taskSchemaLegacy       = 2
	taskSchemaCurrent      = 3
	inFlightSchemaPrevious = 1
	inFlightSchemaCurrent  = 2

	RootKind      Kind  = "root"
	BlobKind      Kind  = "blob"
	Pending       State = "pending"
	Running       State = "running"
	RetryWait     State = "retry_wait"
	WaitingCache  State = "waiting_cache"
	WaitingDeps   State = "waiting_dependencies"
	WaitingQuota  State = "waiting_capacity"
	VerifiedLocal State = "verified_local"
	Solidified    State = "solidified"
	Failed        State = "failed"
	Cancelled     State = "cancelled"
	RepairPending State = "repair_pending"
)

type Task struct {
	SchemaVersion           int                   `json:"schemaVersion"`
	ProtocolVersion         int                   `json:"protocolVersion,omitempty"`
	ID                      string                `json:"id"`
	Kind                    Kind                  `json:"kind,omitempty"`
	Authority               string                `json:"authority"`
	Repository              string                `json:"repository"`
	ScopeID                 string                `json:"scopeID,omitempty"`
	RoundID                 string                `json:"roundID,omitempty"`
	ProtectionSessionID     string                `json:"protectionSessionID,omitempty"`
	ProtectionOwnerID       string                `json:"protectionOwnerID,omitempty"`
	RootDigest              string                `json:"rootDigest,omitempty"`
	BlobDigest              string                `json:"blobDigest,omitempty"`
	BlobSize                int64                 `json:"blobSize,omitempty"`
	CacheVersion            string                `json:"cacheVersion,omitempty"`
	MediaType               string                `json:"mediaType,omitempty"`
	Discovery               string                `json:"discovery"`
	Reference               string                `json:"reference"`
	Generation              uint64                `json:"generation"`
	GraphVersion            uint64                `json:"graphVersion"`
	Graph                   map[string]GraphEntry `json:"graph,omitempty"`
	Checkpoints             map[string]string     `json:"checkpoints,omitempty"`
	RetryCount              int                   `json:"retryCount"`
	NextAttemptAt           time.Time             `json:"nextAttemptAt,omitempty"`
	CreatedAt               time.Time             `json:"createdAt"`
	UpdatedAt               time.Time             `json:"updatedAt"`
	Status                  State                 `json:"status"`
	WaitReason              string                `json:"waitReason,omitempty"`
	QuotaWaitAt             time.Time             `json:"quotaWaitAt,omitempty"`
	QuotaWaitReason         string                `json:"quotaWaitReason,omitempty"`
	QuotaWaitBytes          int64                 `json:"quotaWaitBytes,omitempty"`
	QuotaReplaceableBytes   int64                 `json:"quotaReplaceableBytes,omitempty"`
	QuotaReplaceableSlots   int                   `json:"quotaReplaceableSlots,omitempty"`
	Phase                   string                `json:"phase"`
	LastProgressAt          time.Time             `json:"lastProgressAt,omitempty"`
	ExpiresAt               time.Time             `json:"expiresAt,omitempty"`
	CancelRequested         bool                  `json:"cancelRequested,omitempty"`
	CancelPhase             string                `json:"cancelPhase,omitempty"`
	SessionIDs              []string              `json:"sessionIDs,omitempty"`
	RequestedBytes          int64                 `json:"requestedBytes,omitempty"`
	ReservedBytes           int64                 `json:"reservedBytes,omitempty"`
	LegacyProtectionBytes   int64                 `json:"legacyProtectionBytes,omitempty"`
	ProtectedCacheBytes     int64                 `json:"protectedCacheBytes,omitempty"`
	GCProtected             bool                  `json:"gcProtected,omitempty"`
	RecoveryReason          string                `json:"recoveryReason,omitempty"`
	RootPublishing          bool                  `json:"rootPublishing,omitempty"`
	RootPublished           bool                  `json:"rootPublished,omitempty"`
	CleanupStarted          bool                  `json:"cleanupStarted,omitempty"`
	TerminalCleanupComplete bool                  `json:"terminalCleanupComplete,omitempty"`
	Retryable               bool                  `json:"retryable,omitempty"`
	ErrorClass              string                `json:"errorClass,omitempty"`
	ErrorMessage            string                `json:"errorMessage,omitempty"`
	FencingToken            uint64                `json:"fencingToken,omitempty"`
	version                 string
}

type Failure struct {
	Class         string
	Permanent     bool
	RepairPending bool
	Err           error
}

type Yield struct {
	Status State
	Reason string
}

func (y *Yield) Error() string { return "task yielded to " + string(y.Status) }

func WaitFor(status State, reason string) error {
	switch status {
	case WaitingCache, WaitingDeps, WaitingQuota, VerifiedLocal:
		if !validWaitReason(reason) {
			return errors.New("task wait reason must be a stable lowercase identifier")
		}
		return &Yield{Status: status, Reason: reason}
	default:
		return errors.New("unsupported task wait state")
	}
}

func validWaitReason(reason string) bool {
	if len(reason) == 0 || len(reason) > 64 {
		return false
	}
	for _, char := range reason {
		if char < 'a' || char > 'z' {
			if char < '0' || char > '9' {
				if char != '_' {
					return false
				}
			}
		}
	}
	return true
}

func isWaiting(status State) bool {
	return status == WaitingCache || status == WaitingDeps || status == WaitingQuota || status == VerifiedLocal
}

func isActive(status State) bool {
	return status == Pending || status == Running || status == RetryWait || isWaiting(status) || status == RepairPending
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

type Processor interface {
	Process(context.Context, *Task, lease.Lease) error
}

type TaskTerminalHandler interface {
	TaskTerminal(context.Context, *Task) error
}

type TaskYieldedHandler interface {
	TaskYielded(context.Context, *Task)
}

type Manager struct {
	store           storage.Store
	leases          *lease.Manager
	processor       Processor
	workers         int
	pendingMax      int
	maxWaitingRoots int
	idleTimeout     time.Duration
	mu              sync.Mutex
	active          map[string]struct{}
	cancels         map[string]context.CancelFunc
	owner           string
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

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

func (m *Manager) SetRecoveryReason(ctx context.Context, id, reason string) error {
	if len(reason) > 64 || !validWaitReason(reason) {
		return errors.New("task recovery reason is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.RecoveryReason == reason {
			return nil
		}
		task.RecoveryReason = reason
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (m *Manager) SetProtectedCacheBytes(ctx context.Context, id string, bytes int64) error {
	if bytes < 0 {
		return errors.New("task protected cache bytes cannot be negative")
	}
	for attempt := 0; attempt < 16; attempt++ {
		task, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if task.Kind != BlobKind {
			return errors.New("protected cache bytes can only be recorded on a blob task")
		}
		if task.ProtectedCacheBytes == bytes {
			return nil
		}
		task.ProtectedCacheBytes = bytes
		task.UpdatedAt = time.Now().UTC()
		if err := m.save(ctx, &task); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}
