package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

// CleanupOrphans removes stale cache records that never reached the complete state.
// Corrupt records are left in place and returned as an error so recovery cannot
// mistake an unreadable object for an unreferenced object.
func (c *Cache) CleanupOrphans(ctx context.Context, now time.Time, olderThan time.Duration, limit int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if olderThan <= 0 {
		return 0, errors.New("orphan retention must be positive")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	cutoff := now.Add(-olderThan)
	removed := 0
	token := ""
	for removed < limit {
		entries, next, err := c.store.List(ctx, "cache/objects/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if removed >= limit || entry.ModTime == 0 || time.Unix(0, entry.ModTime).After(cutoff) {
				continue
			}
			var changed bool
			switch {
			case strings.Contains(entry.Key, "/representations/") && strings.HasSuffix(entry.Key, ".json"):
				changed, err = c.cleanupOrphanMetadata(ctx, entry)
			case strings.Contains(entry.Key, "/complete/") && strings.HasSuffix(entry.Key, ".json"):
				changed, err = c.cleanupOrphanComplete(ctx, entry)
			case strings.HasSuffix(entry.Key, "/data"):
				changed, err = c.cleanupOrphanData(ctx, entry)
			}
			if err != nil {
				return removed, err
			}
			if changed {
				removed++
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}

func (c *Cache) cleanupOrphanMetadata(ctx context.Context, entry storage.Entry) (bool, error) {
	identityEnd := strings.Index(entry.Key, "/representations/")
	if identityEnd < len("cache/objects/") {
		return false, nil
	}
	identity := strings.TrimPrefix(entry.Key[:identityEnd], "cache/objects/")
	metadata, version, err := c.readMetadataEntry(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, errors.Join(storage.ErrCorruptObject, err)
	}
	if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Authority == "" || metadata.Repository == "" || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 || metadata.DataVersion == "" || metadata.State != "complete" && metadata.State != "cleanup_pending" {
		return false, storage.ErrCorruptObject
	}
	if err := c.verifyComplete(ctx, identity, metadata); err == nil {
		return false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, errors.Join(storage.ErrCorruptObject, err)
	}
	protected, err := c.isProtected(ctx, metadata.Authority, metadata.Repository, metadata.Digest)
	if err != nil {
		return false, err
	}
	if protected {
		return false, nil
	}
	if err := c.store.DeleteIfMatch(ctx, entry.Key, version); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := c.updateRepresentationIndex(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaType, false); err != nil {
		return true, err
	}
	if err := c.removeDataIfUnreferenced(ctx, identity, metadata.DataVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return true, err
	}
	return true, nil
}

func (c *Cache) cleanupOrphanComplete(ctx context.Context, entry storage.Entry) (bool, error) {
	identityEnd := strings.Index(entry.Key, "/complete/")
	if identityEnd < len("cache/objects/") {
		return false, nil
	}
	identity := strings.TrimPrefix(entry.Key[:identityEnd], "cache/objects/")
	marker, err := c.readCompleteKey(ctx, entry.Key)
	if err != nil {
		return false, errors.Join(storage.ErrCorruptObject, err)
	}
	if marker.SchemaVersion != cacheSchemaCurrent || marker.Authority == "" || marker.Repository == "" || marker.Digest == "" || marker.MediaType == "" || marker.Size < 0 || marker.DataVersion == "" {
		return false, storage.ErrCorruptObject
	}
	metadataKey := cacheMetadataKey(identity, marker.MediaType)
	if _, err := c.store.Head(ctx, metadataKey); err == nil {
		return false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	protected, err := c.isProtected(ctx, marker.Authority, marker.Repository, marker.Digest)
	if err != nil {
		return false, err
	}
	if protected {
		return false, nil
	}
	if err := c.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := c.updateRepresentationIndex(ctx, marker.Authority, marker.Repository, marker.Digest, marker.MediaType, false); err != nil {
		return true, err
	}
	if err := c.removeDataIfUnreferenced(ctx, identity, marker.DataVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return true, err
	}
	return true, nil
}

func (c *Cache) isProtected(ctx context.Context, authority, repository, digest string) (bool, error) {
	if c.protectOrphans == nil {
		return false, nil
	}
	return c.protectOrphans(ctx, authority, repository, digest)
}

func (c *Cache) cleanupOrphanData(ctx context.Context, entry storage.Entry) (bool, error) {
	identity := strings.TrimPrefix(strings.TrimSuffix(entry.Key, "/data"), "cache/objects/")
	if identity == "" || strings.Contains(identity, "/") {
		return false, nil
	}
	representations, _, err := c.store.List(ctx, "cache/objects/"+identity+"/representations/", "", 1)
	if err != nil {
		return false, err
	}
	if len(representations) > 0 {
		return false, nil
	}
	complete, _, err := c.store.List(ctx, "cache/objects/"+identity+"/complete/", "", 1)
	if err != nil {
		return false, err
	}
	if len(complete) > 0 {
		return false, nil
	}
	if err := c.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (c *Cache) removeDataIfUnreferenced(ctx context.Context, identity, expectedVersion string) error {
	entries, _, err := c.store.List(ctx, "cache/objects/"+identity+"/representations/", "", 2)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	dataKey := "cache/objects/" + identity + "/data"
	entry, err := c.store.Head(ctx, dataKey)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if expectedVersion != "" && entry.Version != expectedVersion {
		return nil
	}
	return c.store.DeleteIfMatch(ctx, dataKey, entry.Version)
}

func (c *Cache) readMetadata(ctx context.Context, key string) (cacheMetadata, error) {
	metadata, _, err := c.readMetadataEntry(ctx, key)
	return metadata, err
}
