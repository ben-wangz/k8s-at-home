package download

import (
	"context"
	"net/http"
	"time"

	"registry-hub/internal/storage"
)

const (
	stateSchemaVersion     = 1
	indexPageLimit         = 256
	defaultChunkSize       = 8 << 20
	defaultFinalizeTimeout = 300 * time.Second
	maxNoProgressRetries   = 3
	partialPrefix          = "downloads/v1/"
)

type chunk struct {
	Key     string `json:"key"`
	Start   int64  `json:"start"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	Version string `json:"version"`
}

type indexPage struct {
	Previous string  `json:"previous,omitempty"`
	Chunks   []chunk `json:"chunks"`
}

type state struct {
	Authority       string    `json:"authority,omitempty"`
	Repository      string    `json:"repository,omitempty"`
	SchemaVersion   int       `json:"schemaVersion"`
	Identity        string    `json:"identity"`
	Digest          string    `json:"digest"`
	Session         string    `json:"session"`
	OwnerFence      uint64    `json:"ownerFence"`
	Revision        uint64    `json:"revision"`
	ExpectedSize    int64     `json:"expectedSize"`
	TotalSize       int64     `json:"totalSize"`
	Offset          int64     `json:"offset"`
	Index           string    `json:"index,omitempty"`
	ETag            string    `json:"etag,omitempty"`
	RangeDisabled   bool      `json:"rangeDisabled,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	ProgressAt      time.Time `json:"progressAt"`
	Fallbacks       int       `json:"fallbacks"`
	Reservation     int64     `json:"reservation"`
	GarbageSessions []string  `json:"garbageSessions,omitempty"`
}

type stateVersion struct {
	Value   state
	Version string
}

type Manager struct {
	store       storage.Store
	chunkSize   int64
	maxReserved int64
	now         func() time.Time
	quotaWait   func(context.Context, int) error
}

type Request struct {
	Authority        string
	Repository       string
	Identity         string
	Digest           string
	ExpectedSize     int64
	MaxBytes         int64
	OwnerFence       uint64
	Guard            func(context.Context) error
	AttemptLimit     time.Duration
	IdleTimeout      time.Duration
	TotalTimeout     time.Duration
	FinalizeTimeout  time.Duration
	LifecycleContext context.Context
	Source           func(context.Context, int64, int64, string) (*http.Response, context.CancelFunc, error)
	WaitRetry        func(context.Context, string, int) error
	Retryable        func(error) bool
	Observe          func(string)
}

type Result struct {
	Size        int64
	MediaType   string
	ETag        string
	ResumedFrom int64
}

func New(store storage.Store, maxReserved int64) *Manager {
	if maxReserved <= 0 {
		maxReserved = 200 << 30
	}
	return &Manager{store: store, chunkSize: defaultChunkSize, maxReserved: maxReserved, now: time.Now, quotaWait: waitQuotaConflict}
}
