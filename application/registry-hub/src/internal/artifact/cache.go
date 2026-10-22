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
	SchemaVersion int       `json:"schemaVersion"`
	Authority     string    `json:"authority"`
	Repository    string    `json:"repository"`
	Digest        string    `json:"digest"`
	MediaType     string    `json:"mediaType"`
	Size          int64     `json:"size"`
	DataVersion   string    `json:"dataVersion"`
	VerifiedAt    time.Time `json:"verifiedAt"`
	CleanupAfter  time.Time `json:"cleanupAfter,omitempty"`
	State         string    `json:"state"`
}

type cacheComplete struct {
	SchemaVersion int       `json:"schemaVersion"`
	Authority     string    `json:"authority"`
	Repository    string    `json:"repository"`
	Digest        string    `json:"digest"`
	MediaType     string    `json:"mediaType"`
	Size          int64     `json:"size"`
	DataVersion   string    `json:"dataVersion"`
	CompletedAt   time.Time `json:"completedAt"`
}

type cacheRepresentationIndex struct {
	SchemaVersion int      `json:"schemaVersion"`
	Authority     string   `json:"authority"`
	Repository    string   `json:"repository"`
	Digest        string   `json:"digest"`
	MediaTypes    []string `json:"mediaTypes"`
}

type CachedObject struct {
	Authority   string
	Repository  string
	Digest      string
	MediaType   string
	Size        int64
	DataVersion string
	Body        io.ReadCloser
}

type CachedRepresentation struct {
	Authority   string
	Repository  string
	Digest      string
	MediaType   string
	Size        int64
	DataVersion string
	State       string
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
	cacheRepresentationIndexMaxSize       = 1 << 20
	cacheRepresentationIndexMaxMediaTypes = 1000
)

type Cache struct {
	store          storage.Store
	mu             sync.Mutex
	identityLocks  [256]sync.Mutex
	protectOrphans func(context.Context, string, string, string) (bool, error)
}

func NewCache(store storage.Store) *Cache { return &Cache{store: store} }

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
