package artifact

import (
	"bytes"
	"context"
	"errors"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

func (c *Cache) getAny(ctx context.Context, authority, repository, digest string, _ bool) (CachedObject, error) {
	return c.getAnyMode(ctx, authority, repository, digest, true, false)
}

func (c *Cache) getAnyMode(ctx context.Context, authority, repository, digest string, consume, forceVerify bool) (CachedObject, error) {
	identity := cacheIdentity(authority, repository, digest)
	index, _, err := c.readRepresentationIndex(ctx, identity, authority, repository, digest)
	if err != nil {
		return CachedObject{}, withCacheReadFailureStage("representation_index_read", err)
	}
	for _, mediaType := range index.MediaTypes {
		metadataKey := cacheMetadataKey(identity, mediaType)
		metadata, metadataVersion, readErr := c.readMetadataEntry(ctx, metadataKey)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if errors.Is(readErr, storage.ErrCorruptObject) {
			if invalidateErr := c.invalidateCorruptRepresentation(ctx, authority, repository, digest, mediaType, metadataVersion); invalidateErr != nil {
				return CachedObject{}, withCacheReadFailureStage("corrupt_representation_repair", errors.Join(readErr, invalidateErr))
			}
			continue
		}
		if readErr != nil {
			return CachedObject{}, withCacheReadFailureStage("representation_metadata_read", readErr)
		}
		if metadata.SchemaVersion != cachePayloadSchemaCurrent || metadata.Authority != authority || metadata.Repository != repository || metadata.Digest != digest || metadata.MediaType != mediaType {
			return CachedObject{}, withCacheReadFailureStage("representation_metadata_validate", storage.ErrCorruptObject)
		}
		cleanupPending := metadata.State == "cleanup_pending"
		if metadata.State != "complete" && !cleanupPending {
			return CachedObject{}, withCacheReadFailureStage("representation_metadata_validate", storage.ErrCorruptObject)
		}
		var object CachedObject
		var getErr error
		if forceVerify {
			object, getErr = c.openVerifiedMode(ctx, authority, repository, digest, mediaType, consume, true)
		} else {
			object, getErr = c.openVerified(ctx, authority, repository, digest, mediaType, consume)
		}
		if errors.Is(getErr, storage.ErrNotFound) {
			stage := CacheReadFailureStage(getErr)
			if stage == "" {
				stage = "indexed_object_read"
			}
			return CachedObject{}, withCacheReadFailureStage(stage+"_missing", errors.Join(storage.ErrUnavailable, errors.New("cache representation index references an incomplete object")))
		}
		if errors.Is(getErr, storage.ErrCorruptObject) {
			if removeErr := c.invalidateCorruptRepresentation(ctx, authority, repository, digest, mediaType, metadataVersion); removeErr != nil {
				return CachedObject{}, withCacheReadFailureStage("corrupt_representation_repair", errors.Join(getErr, removeErr))
			}
			continue
		}
		return object, withCacheReadFailureStage("payload_open", getErr)
	}
	return CachedObject{}, storage.ErrNotFound
}

func cacheCleanupDeadline(metadata cacheMetadata) time.Time {
	if !metadata.CleanupAfter.IsZero() {
		return metadata.CleanupAfter
	}
	if !metadata.VerifiedAt.IsZero() {
		return metadata.VerifiedAt.Add(24 * time.Hour)
	}
	return time.Time{}
}

func (c *Cache) PutBytes(ctx context.Context, authority, repository, mediaType string, body []byte) (string, error) {
	digest := CalculateSHA256(body)
	if err := c.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		return "", err
	}
	return digest, nil
}

func (c *Cache) Remove(ctx context.Context, authority, repository, digest, mediaType string) error {
	identity := cacheIdentity(authority, repository, digest)
	unlock := c.lockIdentity(identity)
	defer unlock()
	metaKey := cacheMetadataKey(identity, mediaType)
	metadata, version, err := c.readMetadataEntry(ctx, metaKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err == nil {
		completeKey := cacheCompleteKey(identity, mediaType)
		if complete, completeErr := c.store.Head(ctx, completeKey); completeErr == nil {
			if err := c.store.DeleteIfMatch(ctx, completeKey, complete.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else if !errors.Is(completeErr, storage.ErrNotFound) {
			return completeErr
		}
		if err := c.store.DeleteIfMatch(ctx, metaKey, version); err != nil {
			return err
		}
		if err := c.updateRepresentationIndex(ctx, authority, repository, digest, mediaType, false); err != nil {
			return err
		}
		return c.removeDataIfUnreferenced(ctx, identity, metadata.DataVersion)
	}
	return c.updateRepresentationIndex(ctx, authority, repository, digest, mediaType, false)
}

// RebuildRepresentationIndexes restores the read index from verified cache metadata.
func (c *Cache) RebuildRepresentationIndexes(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	rebuilt := make(map[string]cacheRepresentationIndex)
	known := make(map[string]struct{})
	token := ""
	for {
		entries, next, err := c.store.List(ctx, "cache/objects/", token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if identity, ok := cacheRepresentationIndexIdentity(entry.Key); ok {
				known[identity] = struct{}{}
			}
			if !strings.Contains(entry.Key, "/representations/") || !strings.HasSuffix(entry.Key, ".json") {
				continue
			}
			identityEnd := strings.Index(entry.Key, "/representations/")
			if identityEnd < len("cache/objects/") {
				continue
			}
			identity := strings.TrimPrefix(entry.Key[:identityEnd], "cache/objects/")
			metadata, _, readErr := c.readMetadataEntry(ctx, entry.Key)
			if errors.Is(readErr, storage.ErrNotFound) {
				continue
			}
			if readErr != nil {
				return errors.Join(storage.ErrCorruptObject, readErr)
			}
			if metadata.SchemaVersion != cachePayloadSchemaCurrent || metadata.Authority == "" || metadata.Repository == "" || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 || metadata.DataVersion == "" || cacheIdentity(metadata.Authority, metadata.Repository, metadata.Digest) != identity {
				return storage.ErrCorruptObject
			}
			if _, err := Hasher(metadata.Digest); err != nil {
				return errors.Join(storage.ErrCorruptObject, err)
			}
			if metadata.State != "complete" && metadata.State != "cleanup_pending" {
				continue
			}
			if err := c.verifyComplete(ctx, identity, metadata); errors.Is(err, storage.ErrNotFound) {
				continue
			} else if err != nil {
				return errors.Join(storage.ErrCorruptObject, err)
			}
			index, exists := rebuilt[identity]
			if !exists {
				index = cacheRepresentationIndex{SchemaVersion: cacheSchemaCurrent, Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest}
			}
			index.MediaTypes = append(index.MediaTypes, metadata.MediaType)
			rebuilt[identity] = index
			known[identity] = struct{}{}
		}
		if next == "" {
			break
		}
		token = next
	}

	for identity, index := range rebuilt {
		sort.Strings(index.MediaTypes)
		if err := c.replaceRepresentationIndex(ctx, identity, index); err != nil {
			return err
		}
	}
	for identity := range known {
		if _, exists := rebuilt[identity]; exists {
			continue
		}
		if err := c.deleteRepresentationIndex(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}
