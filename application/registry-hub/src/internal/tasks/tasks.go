package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
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
