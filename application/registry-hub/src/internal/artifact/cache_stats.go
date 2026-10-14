package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *Cache) Stats(ctx context.Context, orphanAfter time.Duration) (CacheStats, error) {
	if orphanAfter <= 0 {
		return CacheStats{}, errors.New("cache orphan age must be positive")
	}
	type cacheRecord struct {
		lastModified int64
		committed    bool
		cleanup      bool
	}
	type representationRecord struct {
		mediaType string
		state     string
	}
	data := make(map[string]storage.Entry)
	records := make(map[string]*cacheRecord)
	metadataByID := make(map[string][]representationRecord)
	complete := make(map[string]struct{})
	token := ""
	for {
		entries, next, err := c.store.List(ctx, "cache/objects/", token, 500)
		if err != nil {
			return CacheStats{}, err
		}
		for _, entry := range entries {
			identity, ok := cacheEntryIdentity(entry.Key)
			if !ok {
				continue
			}
			record := records[identity]
			if record == nil {
				record = &cacheRecord{}
				records[identity] = record
			}
			if entry.ModTime > record.lastModified {
				record.lastModified = entry.ModTime
			}
			switch {
			case strings.HasSuffix(entry.Key, "/data"):
				data[identity] = entry
			case strings.Contains(entry.Key, "/complete/") && strings.HasSuffix(entry.Key, ".json"):
				complete[entry.Key] = struct{}{}
			case strings.Contains(entry.Key, "/representations/") && strings.HasSuffix(entry.Key, ".json"):
				metadata, _, readErr := c.readMetadataEntry(ctx, entry.Key)
				if errors.Is(readErr, storage.ErrNotFound) {
					continue
				}
				if readErr != nil {
					return CacheStats{}, readErr
				}
				if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 {
					return CacheStats{}, storage.ErrCorruptObject
				}
				metadataByID[identity] = append(metadataByID[identity], representationRecord{mediaType: metadata.MediaType, state: metadata.State})
			}
		}
		if next == "" {
			break
		}
		token = next
	}

	for identity, representations := range metadataByID {
		record := records[identity]
		for _, metadata := range representations {
			if _, ok := complete[cacheCompleteKey(identity, metadata.mediaType)]; ok && (metadata.state == "complete" || metadata.state == "cleanup_pending") {
				record.committed = true
				record.cleanup = record.cleanup || metadata.state == "cleanup_pending"
			}
		}
	}
	stats := CacheStats{}
	cutoff := time.Now().Add(-orphanAfter).UnixNano()
	incomplete := make(map[string]struct{})
	for identity, entry := range data {
		record := records[identity]
		if record != nil && record.committed {
			stats.Objects++
			stats.Bytes += entry.Size
			if record.cleanup {
				stats.CleanupPending++
			}
			continue
		}
		incomplete[identity] = struct{}{}
	}
	for identity, record := range records {
		if !record.committed {
			incomplete[identity] = struct{}{}
		}
	}
	for identity := range incomplete {
		record := records[identity]
		if record != nil && record.lastModified != 0 && record.lastModified <= cutoff {
			stats.Orphans++
		} else {
			stats.Staging++
		}
	}
	return stats, nil
}

func cacheEntryIdentity(key string) (string, bool) {
	identity := cacheEntryIdentityValue(key)
	return identity, identity != "" && !strings.Contains(identity, "/")
}

func cacheEntryIdentityValue(key string) string {
	if !strings.HasPrefix(key, "cache/objects/") {
		return ""
	}
	remainder := strings.TrimPrefix(key, "cache/objects/")
	identity, _, found := strings.Cut(remainder, "/")
	if !found {
		return ""
	}
	return identity
}

func (c *Cache) Cleanup(ctx context.Context, now time.Time, limit int) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	removed := 0
	token := ""
	for removed < limit {
		entries, next, err := c.store.List(ctx, "cache/objects/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if !strings.Contains(entry.Key, "/representations/") || !strings.HasSuffix(entry.Key, ".json") {
				continue
			}
			metadata, version, readErr := c.readMetadataEntry(ctx, entry.Key)
			if errors.Is(readErr, storage.ErrNotFound) {
				continue
			}
			if readErr != nil {
				return removed, readErr
			}
			identityEnd := strings.Index(entry.Key, "/representations/")
			if identityEnd < len("cache/objects/") {
				continue
			}
			identity := strings.TrimPrefix(entry.Key[:identityEnd], "cache/objects/")
			complete, completeErr := c.readComplete(ctx, identity, metadata.MediaType)
			if errors.Is(completeErr, storage.ErrNotFound) {
				continue
			}
			if completeErr != nil {
				return removed, completeErr
			}
			if complete.DataVersion != metadata.DataVersion || complete.Size != metadata.Size || complete.Digest != metadata.Digest || complete.MediaType != metadata.MediaType {
				return removed, errors.New("cache complete marker does not match metadata")
			}
			deadline := cacheCleanupDeadline(metadata)
			if (metadata.State != "complete" && metadata.State != "cleanup_pending") || deadline.IsZero() || deadline.After(now) {
				continue
			}
			protected, err := c.isProtected(ctx, metadata.Authority, metadata.Repository, metadata.Digest)
			if err != nil {
				return removed, err
			}
			if protected {
				continue
			}
			if err := c.store.DeleteIfMatch(ctx, entry.Key, version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			if err := c.updateRepresentationIndex(ctx, metadata.Authority, metadata.Repository, metadata.Digest, metadata.MediaType, false); err != nil {
				return removed, err
			}
			if completeEntry, completeErr := c.store.Head(ctx, cacheCompleteKey(identity, metadata.MediaType)); completeErr == nil {
				if completeErr = c.store.DeleteIfMatch(ctx, cacheCompleteKey(identity, metadata.MediaType), completeEntry.Version); completeErr != nil && !errors.Is(completeErr, storage.ErrNotFound) {
					return removed, completeErr
				}
			} else if !errors.Is(completeErr, storage.ErrNotFound) {
				return removed, completeErr
			}
			if err := c.removeDataIfUnreferenced(ctx, identity, metadata.DataVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return removed, err
			}
			removed++
			if removed >= limit {
				return removed, nil
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}
