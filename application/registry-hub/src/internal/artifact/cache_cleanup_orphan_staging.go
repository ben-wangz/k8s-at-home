package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *Cache) cleanupOrphanStaging(ctx context.Context, entry storage.Entry) (bool, error) {
	identity, stagingKey, found := strings.Cut(strings.TrimPrefix(entry.Key, "cache/objects/"), "/staging/")
	if !found || identity == "" || stagingKey == "" || strings.Contains(stagingKey, "/") {
		return false, nil
	}
	pinned, err := c.hasActiveStagingPin(ctx, identity, time.Now().UTC())
	if err != nil || pinned {
		return false, err
	}
	if err := c.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
