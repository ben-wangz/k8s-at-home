package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (c *Cache) CleanupReaderPins(ctx context.Context, limit int) (int, error) {
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	removed := 0
	for _, prefix := range []string{"cache/readers/", "cache/staging-readers/"} {
		token := ""
		for removed < limit {
			entries, next, err := c.store.List(ctx, prefix, token, 500)
			if err != nil {
				return removed, err
			}
			for _, entry := range entries {
				if removed >= limit {
					break
				}
				if deleted, err := c.cleanupReaderPin(ctx, entry); err != nil {
					return removed, err
				} else if deleted {
					removed++
				}
			}
			if next == "" {
				break
			}
			token = next
		}
	}
	return removed, nil
}

func (c *Cache) cleanupReaderPin(ctx context.Context, entry storage.Entry) (bool, error) {
	object, getErr := c.store.Get(ctx, entry.Key)
	if errors.Is(getErr, storage.ErrNotFound) {
		return false, nil
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
	if err := json.Unmarshal(data, &pin); err != nil || pin.SchemaVersion != cacheSchemaCurrent || pin.Identity == "" || pin.ID == "" || pin.ExpiresAt.IsZero() {
		return false, errors.New("cache reader pin is invalid")
	}
	if pin.ExpiresAt.After(time.Now().UTC()) {
		return false, nil
	}
	if err := c.store.DeleteIfMatch(ctx, entry.Key, object.Version); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
