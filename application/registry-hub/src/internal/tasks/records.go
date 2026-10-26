package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"time"
)

const solidificationRecordSchema = 1

type BlobIntentState string

const (
	IntentWaitingCache BlobIntentState = "waiting_cache"
	IntentWaitingQuota BlobIntentState = "waiting_capacity"
	IntentReady        BlobIntentState = "ready"
	IntentRunning      BlobIntentState = "running"
	IntentRetryWait    BlobIntentState = "retry_wait"
	IntentVerified     BlobIntentState = "verified_local"
	IntentAnchored     BlobIntentState = "anchored"
	IntentFailed       BlobIntentState = "failed"
	IntentCancelled    BlobIntentState = "cancelled"
)

type BlobIdentity struct {
	Authority  string `json:"authority"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

type BlobIntent struct {
	SchemaVersion       int             `json:"schemaVersion"`
	ID                  string          `json:"id"`
	Identity            BlobIdentity    `json:"identity"`
	Round               uint64          `json:"round"`
	Generation          uint64          `json:"generation"`
	MediaType           string          `json:"mediaType,omitempty"`
	State               BlobIntentState `json:"state"`
	WakeAcknowledged    bool            `json:"wakeAcknowledged,omitempty"`
	CacheVersion        string          `json:"cacheVersion,omitempty"`
	ActualSize          int64           `json:"actualSize,omitempty"`
	ProtectionSessionID string          `json:"protectionSessionID,omitempty"`
	RetryCount          int             `json:"retryCount"`
	NextAttemptAt       time.Time       `json:"nextAttemptAt,omitempty"`
	LastProgressAt      time.Time       `json:"lastProgressAt"`
	ExpiresAt           time.Time       `json:"expiresAt,omitempty"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	CancelRequested     bool            `json:"cancelRequested,omitempty"`
	CancelPhase         string          `json:"cancelPhase,omitempty"`
	FencingToken        uint64          `json:"fencingToken,omitempty"`
	ErrorClass          string          `json:"errorClass,omitempty"`
	ErrorMessage        string          `json:"errorMessage,omitempty"`
	version             string
}

type GraphRole string

const (
	GraphBlob     GraphRole = "blob"
	GraphManifest GraphRole = "manifest"
)

type GraphNode struct {
	SchemaVersion     int       `json:"schemaVersion"`
	RootTaskID        string    `json:"rootTaskID"`
	Role              GraphRole `json:"role"`
	Digest            string    `json:"digest"`
	MediaType         string    `json:"mediaType"`
	DeclaredSize      int64     `json:"declaredSize"`
	ActualSize        int64     `json:"actualSize,omitempty"`
	DiscoveryComplete bool      `json:"discoveryComplete,omitempty"`
	LocalState        string    `json:"localState,omitempty"`
	Generation        uint64    `json:"generation"`
	ContentGeneration *uint64   `json:"contentGeneration,omitempty"`
	GCEpoch           uint64    `json:"gcEpoch"`
	version           string
}

type DependencyEdge struct {
	SchemaVersion int        `json:"schemaVersion"`
	RootTaskID    string     `json:"rootTaskID"`
	ParentRole    GraphRole  `json:"parentRole"`
	ParentDigest  string     `json:"parentDigest"`
	Order         int        `json:"order"`
	ChildRole     GraphRole  `json:"childRole"`
	ChildDigest   string     `json:"childDigest"`
	Descriptor    GraphEntry `json:"descriptor"`
}

type LocalObservation struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Authority         string    `json:"authority"`
	Repository        string    `json:"repository"`
	Role              GraphRole `json:"role"`
	Digest            string    `json:"digest"`
	ActualSize        int64     `json:"actualSize"`
	ObservedAt        time.Time `json:"observedAt"`
	Generation        uint64    `json:"generation"`
	GCEpoch           uint64    `json:"gcEpoch"`
	OperationID       string    `json:"operationID"`
	VerificationBasis string    `json:"verificationBasis"`
}

type Records struct {
	store          storage.Store
	maxBlobIntents int
	idleTimeout    time.Duration
	capacityMu     sync.Mutex
}

func NewRecords(store storage.Store) *Records {
	return &Records{store: store, maxBlobIntents: 10000, idleTimeout: 24 * time.Hour}
}

func (r *Records) SetMaxBlobIntents(limit int) {
	if limit > 0 {
		r.maxBlobIntents = limit
	}
}

func (r *Records) SetIdleTimeout(timeout time.Duration) {
	if timeout > 0 {
		r.idleTimeout = timeout
	}
}

func (r *Records) idleExpiry(now time.Time) time.Time {
	timeout := r.idleTimeout
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	return now.Add(timeout)
}

func decodeRecord(object storage.Object, limit int64, target any) error {
	data, readErr := io.ReadAll(io.LimitReader(object.Body, limit+1))
	closeErr := object.Body.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if int64(len(data)) > limit {
		return errors.New("solidification state record exceeds size limit")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode solidification state record: %w", err)
	}
	return nil
}

func validateBlobIdentity(identity BlobIdentity) error {
	if identity.Authority == "" || len(identity.Authority) > 255 || identity.Repository == "" || len(identity.Repository) > 1024 || identity.Digest == "" || len(identity.Digest) > 512 || strings.ContainsAny(identity.Authority+identity.Repository+identity.Digest, "\r\n\x00") {
		return errors.New("blob identity is invalid")
	}
	return nil
}

func validBlobIntentState(state BlobIntentState) bool {
	switch state {
	case IntentWaitingCache, IntentWaitingQuota, IntentReady, IntentRunning, IntentRetryWait, IntentVerified, IntentAnchored, IntentFailed, IntentCancelled:
		return true
	default:
		return false
	}
}

func blobIntentActive(state BlobIntentState) bool {
	return state == IntentWaitingCache || state == IntentWaitingQuota || state == IntentReady || state == IntentRunning || state == IntentRetryWait || state == IntentVerified
}

func blobIntentConsumesCapacity(state BlobIntentState) bool {
	return state == IntentWaitingCache || state == IntentWaitingQuota || state == IntentReady || state == IntentRunning || state == IntentRetryWait
}

func validGraphRole(role GraphRole) bool { return role == GraphBlob || role == GraphManifest }

func recordHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
