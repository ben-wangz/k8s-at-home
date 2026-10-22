package artifact

import (
	"context"
	"errors"

	"registry-hub/internal/storage"
)

func (c *Cache) invalidateCorruptRepresentation(ctx context.Context, authority, repository, digest, mediaType, expectedVersion string) error {
	identity := cacheIdentity(authority, repository, digest)
	unlock := c.lockIdentity(identity)
	defer unlock()

	metadataKey := cacheMetadataKey(identity, mediaType)
	metadata, err := c.store.Head(ctx, metadataKey)
	if errors.Is(err, storage.ErrNotFound) {
		if err := c.updateRepresentationIndex(ctx, authority, repository, digest, mediaType, false); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if expectedVersion != "" && metadata.Version != expectedVersion {
		return storage.ErrConditionFailed
	}
	if err := deleteCorruptCacheEntry(ctx, c, metadataKey, metadata.Version); err != nil {
		return err
	}
	completeKey := cacheCompleteKey(identity, mediaType)
	if complete, err := c.store.Head(ctx, completeKey); err == nil {
		if err := deleteCorruptCacheEntry(ctx, c, completeKey, complete.Version); err != nil {
			return err
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := c.updateRepresentationIndex(ctx, authority, repository, digest, mediaType, false); err != nil {
		return err
	}
	return c.removeDataIfUnreferenced(ctx, identity, "")
}

func deleteCorruptCacheEntry(ctx context.Context, cache *Cache, key, version string) error {
	err := cache.store.DeleteIfMatch(ctx, key, version)
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	entry, headErr := cache.store.Head(ctx, key)
	if errors.Is(headErr, storage.ErrNotFound) {
		return nil
	}
	if headErr != nil {
		return errors.Join(err, headErr)
	}
	if entry.Version != version {
		return storage.ErrConditionFailed
	}
	return errors.Join(storage.ErrUnknownCommit, err)
}
