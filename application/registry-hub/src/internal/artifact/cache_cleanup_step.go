package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *Cache) CleanupEntry(ctx context.Context, entry storage.Entry, now time.Time) error {
	if strings.Contains(entry.Key, "/representations/") && strings.HasSuffix(entry.Key, ".json") {
		if _, err := c.cleanupExpiredEntry(ctx, entry, now); err != nil {
			return err
		}
	}
	if entry.ModTime != 0 && time.Unix(0, entry.ModTime).Before(now.Add(-time.Hour)) {
		_, err := c.cleanupOrphanEntry(ctx, entry)
		return err
	}
	return nil
}

func (c *Cache) CleanupReaderPinEntry(ctx context.Context, entry storage.Entry) error {
	object, err := c.store.Get(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return err
	}
	var pin cacheReaderPinRecord
	if len(data) > 1<<20 || json.Unmarshal(data, &pin) != nil || pin.SchemaVersion != cacheSchemaCurrent || pin.Identity == "" || pin.ID == "" || pin.ExpiresAt.IsZero() {
		return storage.ErrCorruptObject
	}
	if pin.ExpiresAt.After(time.Now().UTC()) {
		return nil
	}
	err = c.store.DeleteIfMatch(ctx, entry.Key, object.Version)
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}
