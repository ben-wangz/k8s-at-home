package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"registry-hub/internal/storage"
)

const (
	readerPinTTL      = 2 * time.Minute
	readerPinInterval = 30 * time.Second
)

type cacheReaderPin struct {
	key      string
	version  string
	identity string
	id       string
	expires  time.Time
}

type cacheReaderPinRecord struct {
	SchemaVersion int       `json:"schemaVersion"`
	Identity      string    `json:"identity"`
	ID            string    `json:"id"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

type pinnedReader struct {
	cache  *Cache
	body   io.ReadCloser
	ctx    context.Context
	cancel context.CancelFunc
	pin    cacheReaderPin
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	err    error
}

func newPinnedReader(cache *Cache, body io.ReadCloser, ctx context.Context, cancel context.CancelFunc, pin cacheReaderPin) *pinnedReader {
	reader := &pinnedReader{cache: cache, body: body, ctx: ctx, cancel: cancel, pin: pin, done: make(chan struct{})}
	go reader.renew()
	return reader
}

func (c *Cache) createReaderPin(ctx context.Context, identity string) (cacheReaderPin, error) {
	var rawID [16]byte
	if _, err := rand.Read(rawID[:]); err != nil {
		return cacheReaderPin{}, err
	}
	id := hex.EncodeToString(rawID[:])
	pin := cacheReaderPin{identity: identity, id: id, key: "cache/readers/" + identity + "/" + id + ".json", expires: time.Now().UTC().Add(readerPinTTL)}
	record := cacheReaderPinRecord{SchemaVersion: cacheSchemaCurrent, Identity: identity, ID: id, ExpiresAt: pin.expires}
	data, err := json.Marshal(record)
	if err != nil {
		return cacheReaderPin{}, err
	}
	entry, err := c.store.Put(ctx, pin.key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		return cacheReaderPin{}, err
	}
	pin.version = entry.Version
	return pin, nil
}

func (c *Cache) renewReaderPin(pin *cacheReaderPin) error {
	if pin == nil || pin.key == "" || pin.version == "" {
		return errors.New("cache reader pin is incomplete")
	}
	pin.expires = time.Now().UTC().Add(readerPinTTL)
	record := cacheReaderPinRecord{SchemaVersion: cacheSchemaCurrent, Identity: pin.identity, ID: pin.id, ExpiresAt: pin.expires}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entry, err := c.store.Put(ctx, pin.key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: pin.version})
	if err != nil {
		return err
	}
	pin.version = entry.Version
	return nil
}

func (c *Cache) deleteReaderPin(ctx context.Context, pin cacheReaderPin) error {
	if pin.key == "" || pin.version == "" {
		return nil
	}
	err := c.store.DeleteIfMatch(ctx, pin.key, pin.version)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}

func (r *pinnedReader) renew() {
	ticker := time.NewTicker(readerPinInterval)
	defer ticker.Stop()
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
			r.mu.Lock()
			err := r.cache.renewReaderPin(&r.pin)
			if err != nil {
				r.err = err
			}
			r.mu.Unlock()
			if err != nil {
				r.cancel()
				return
			}
		}
	}
}

func (r *pinnedReader) Read(buffer []byte) (int, error) {
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	if err != nil {
		return 0, errors.Join(errors.New("cache reader pin renewal failed"), err)
	}
	return r.body.Read(buffer)
}

func (r *pinnedReader) Close() error {
	var closeErr error
	r.once.Do(func() {
		r.cancel()
		<-r.done
		closeErr = r.body.Close()
		r.mu.Lock()
		pin := r.pin
		r.mu.Unlock()
		pinCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		closeErr = errors.Join(closeErr, r.cache.deleteReaderPin(pinCtx, pin))
	})
	return closeErr
}

func (c *Cache) hasActiveReaderPin(ctx context.Context, identity string, now time.Time) (bool, error) {
	token := ""
	prefix := "cache/readers/" + identity + "/"
	for {
		entries, next, err := c.store.List(ctx, prefix, token, 100)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			object, getErr := c.store.Get(ctx, entry.Key)
			if errors.Is(getErr, storage.ErrNotFound) {
				continue
			}
			if getErr != nil {
				return false, getErr
			}
			data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
			closeErr := object.Body.Close()
			if readErr != nil || closeErr != nil || len(data) > 1<<20 {
				return false, errors.Join(errors.New("cache reader pin could not be read safely"), readErr, closeErr)
			}
			var pin cacheReaderPinRecord
			if err := json.Unmarshal(data, &pin); err != nil || pin.SchemaVersion != cacheSchemaCurrent || pin.Identity != identity || pin.ID == "" || pin.ExpiresAt.IsZero() {
				return false, errors.New("cache reader pin is invalid")
			}
			if pin.ExpiresAt.After(now) {
				return true, nil
			}
			if err := c.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) {
					return true, nil
				}
				if !errors.Is(err, storage.ErrNotFound) {
					return false, err
				}
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func (c *Cache) CleanupReaderPins(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	removed := 0
	token := ""
	for removed < limit {
		entries, next, err := c.store.List(ctx, "cache/readers/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if removed >= limit {
				break
			}
			object, getErr := c.store.Get(ctx, entry.Key)
			if errors.Is(getErr, storage.ErrNotFound) {
				continue
			}
			if getErr != nil {
				return removed, getErr
			}
			data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
			closeErr := object.Body.Close()
			if readErr != nil || closeErr != nil || len(data) > 1<<20 {
				return removed, errors.Join(errors.New("cache reader pin could not be read safely"), readErr, closeErr)
			}
			var pin cacheReaderPinRecord
			if err := json.Unmarshal(data, &pin); err != nil || pin.SchemaVersion != cacheSchemaCurrent || pin.Identity == "" || pin.ID == "" || pin.ExpiresAt.IsZero() {
				return removed, errors.New("cache reader pin is invalid")
			}
			if pin.ExpiresAt.After(time.Now().UTC()) {
				continue
			}
			if err := c.store.DeleteIfMatch(ctx, entry.Key, object.Version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			removed++
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}
