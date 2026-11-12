package artifact

import (
	"context"
	"crypto/sha256"
	"io"
	"registry-hub/internal/storage"
	"sync"
	"time"
)

type cacheMetadata struct {
	SchemaVersion             int       `json:"schemaVersion"`
	Authority                 string    `json:"authority"`
	Repository                string    `json:"repository"`
	Digest                    string    `json:"digest"`
	MediaType                 string    `json:"mediaType"`
	Size                      int64     `json:"size"`
	DataVersion               string    `json:"dataVersion"`
	DataFingerprint           string    `json:"dataFingerprint,omitempty"`
	VerifiedAt                time.Time `json:"verifiedAt"`
	IntegrityState            string    `json:"integrityState,omitempty"`
	IntegrityDigest           string    `json:"integrityDigest,omitempty"`
	IntegritySize             int64     `json:"integritySize,omitempty"`
	IntegrityMethod           string    `json:"integrityMethod,omitempty"`
	IntegrityVerifiedAt       time.Time `json:"integrityVerifiedAt,omitempty"`
	IntegrityBackendAlgorithm string    `json:"integrityBackendAlgorithm,omitempty"`
	IntegrityBackendValue     string    `json:"integrityBackendValue,omitempty"`
	IntegrityBackendScope     string    `json:"integrityBackendScope,omitempty"`
	InventoryTracked          bool      `json:"inventoryTracked,omitempty"`
	CleanupAfter              time.Time `json:"cleanupAfter,omitempty"`
	State                     string    `json:"state"`
}

type cacheComplete struct {
	SchemaVersion             int       `json:"schemaVersion"`
	Authority                 string    `json:"authority"`
	Repository                string    `json:"repository"`
	Digest                    string    `json:"digest"`
	MediaType                 string    `json:"mediaType"`
	Size                      int64     `json:"size"`
	DataVersion               string    `json:"dataVersion"`
	CompletedAt               time.Time `json:"completedAt"`
	IntegrityState            string    `json:"integrityState,omitempty"`
	IntegrityDigest           string    `json:"integrityDigest,omitempty"`
	IntegritySize             int64     `json:"integritySize,omitempty"`
	IntegrityMethod           string    `json:"integrityMethod,omitempty"`
	IntegrityVerifiedAt       time.Time `json:"integrityVerifiedAt,omitempty"`
	IntegrityBackendAlgorithm string    `json:"integrityBackendAlgorithm,omitempty"`
	IntegrityBackendValue     string    `json:"integrityBackendValue,omitempty"`
	IntegrityBackendScope     string    `json:"integrityBackendScope,omitempty"`
}

type cacheRepresentationIndex struct {
	SchemaVersion int      `json:"schemaVersion"`
	Authority     string   `json:"authority"`
	Repository    string   `json:"repository"`
	Digest        string   `json:"digest"`
	MediaTypes    []string `json:"mediaTypes"`
}

type CachedObject struct {
	Authority                 string
	Repository                string
	Digest                    string
	MediaType                 string
	Size                      int64
	DataVersion               string
	IntegrityState            string
	IntegrityMethod           string
	IntegrityVerifiedAt       time.Time
	IntegrityBackendAlgorithm string
	IntegrityBackendValue     string
	IntegrityBackendScope     string
	Body                      io.ReadCloser
}

type CachedRepresentation struct {
	Authority                 string
	Repository                string
	Digest                    string
	MediaType                 string
	Size                      int64
	DataVersion               string
	State                     string
	IntegrityState            string
	IntegrityMethod           string
	IntegrityVerifiedAt       time.Time
	IntegrityBackendAlgorithm string
	IntegrityBackendValue     string
	IntegrityBackendScope     string
}

type CacheStats struct {
	Objects        int64
	Bytes          int64
	Staging        int64
	Orphans        int64
	CleanupPending int64
}

const (
	cacheSchemaPrevious                   = 1
	cacheSchemaCurrent                    = 2
	cachePayloadSchemaPrevious            = 2
	cachePayloadSchemaCurrent             = 3
	cacheRepresentationIndexMaxSize       = 1 << 20
	cacheRepresentationIndexMaxMediaTypes = 1000
)

type Cache struct {
	store             storage.Store
	mu                sync.Mutex
	identityLocks     [256]sync.Mutex
	protectOrphans    func(context.Context, string, string, string) (bool, error)
	retainProjection  func(context.Context, string, string, string, string, time.Time) (bool, error)
	inventoryTracking bool
}

func (c *Cache) EnableInventoryTracking() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inventoryTracking = true
}

func (c *Cache) tracksInventory() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.inventoryTracking }

func NewCache(store storage.Store) *Cache { return &Cache{store: store} }

func (c *Cache) SetProjectionRetention(retain func(context.Context, string, string, string, string, time.Time) (bool, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retainProjection = retain
}

func (c *Cache) SetOrphanProtection(protect func(context.Context, string, string, string) (bool, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.protectOrphans = protect
}

func (c *Cache) Put(ctx context.Context, authority, repository, digest, mediaType string, body io.Reader, size int64) error {
	return c.PutChecked(ctx, authority, repository, digest, mediaType, body, size, nil)
}

func (c *Cache) PutChecked(ctx context.Context, authority, repository, digest, mediaType string, body io.Reader, size int64, beforeComplete func() error) error {
	unlock := c.lockIdentity(cacheIdentity(authority, repository, digest))
	defer unlock()
	return c.put(ctx, authority, repository, digest, mediaType, body, size, beforeComplete)
}

func (c *Cache) lockIdentity(identity string) func() {
	hash := sha256.Sum256([]byte(identity))
	lock := &c.identityLocks[hash[0]]
	lock.Lock()
	return lock.Unlock
}
