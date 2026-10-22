package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

func (c *Cache) updateRepresentationIndex(ctx context.Context, authority, repository, digest, mediaType string, add bool) error {
	identity := cacheIdentity(authority, repository, digest)
	if mediaType == "" || len(mediaType) > 1024 {
		return errors.New("cache representation media type is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		index, version, err := c.readRepresentationIndex(ctx, identity, authority, repository, digest)
		missing := errors.Is(err, storage.ErrNotFound)
		if missing {
			if !add {
				return nil
			}
			index = cacheRepresentationIndex{SchemaVersion: cacheSchemaCurrent, Authority: authority, Repository: repository, Digest: digest}
		} else if err != nil {
			return err
		}

		position := -1
		for i, existing := range index.MediaTypes {
			if existing == mediaType {
				position = i
				break
			}
		}
		if add && position >= 0 || !add && position < 0 {
			return nil
		}
		if add {
			if len(index.MediaTypes) >= cacheRepresentationIndexMaxMediaTypes {
				return storage.ErrUnavailable
			}
			index.MediaTypes = append(index.MediaTypes, mediaType)
			sort.Strings(index.MediaTypes)
		} else {
			index.MediaTypes = append(index.MediaTypes[:position], index.MediaTypes[position+1:]...)
			if len(index.MediaTypes) == 0 {
				deleteErr := c.store.DeleteIfMatch(ctx, cacheRepresentationIndexKey(identity), version)
				if errors.Is(deleteErr, storage.ErrConditionFailed) || errors.Is(deleteErr, storage.ErrUnknownCommit) {
					continue
				}
				if errors.Is(deleteErr, storage.ErrNotFound) {
					return nil
				}
				return deleteErr
			}
		}

		data, err := json.Marshal(index)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: missing}
		if version != "" {
			options = storage.PutOptions{IfMatch: version}
		}
		_, err = c.store.Put(ctx, cacheRepresentationIndexKey(identity), bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}

func (c *Cache) replaceRepresentationIndex(ctx context.Context, identity string, index cacheRepresentationIndex) error {
	if len(index.MediaTypes) == 0 {
		return c.deleteRepresentationIndex(ctx, identity)
	}
	if len(index.MediaTypes) > cacheRepresentationIndexMaxMediaTypes {
		return storage.ErrCorruptObject
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	key := cacheRepresentationIndexKey(identity)
	for attempt := 0; attempt < 16; attempt++ {
		entry, headErr := c.store.Head(ctx, key)
		options := storage.PutOptions{IfAbsent: errors.Is(headErr, storage.ErrNotFound)}
		if headErr != nil && !errors.Is(headErr, storage.ErrNotFound) {
			return headErr
		}
		if headErr == nil {
			options = storage.PutOptions{IfMatch: entry.Version}
		}
		_, putErr := c.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrAlreadyExists) || errors.Is(putErr, storage.ErrUnknownCommit) {
			continue
		}
		return putErr
	}
	return storage.ErrUnavailable
}

func (c *Cache) deleteRepresentationIndex(ctx context.Context, identity string) error {
	key := cacheRepresentationIndexKey(identity)
	for attempt := 0; attempt < 16; attempt++ {
		entry, err := c.store.Head(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		err = c.store.DeleteIfMatch(ctx, key, entry.Version)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		return err
	}
	return storage.ErrUnavailable
}

func cacheRepresentationIndexIdentity(key string) (string, bool) {
	const prefix = "cache/objects/"
	const suffix = "/representation-index.json"
	if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
		return "", false
	}
	identity := strings.TrimSuffix(strings.TrimPrefix(key, prefix), suffix)
	return identity, identity != "" && !strings.Contains(identity, "/")
}

func (c *Cache) MarkCleanupPending(ctx context.Context, authority, repository string, digests []string, cleanupAfter time.Time) error {
	seen := make(map[string]struct{}, len(digests))
	for _, digest := range digests {
		if _, ok := seen[digest]; ok {
			continue
		}
		seen[digest] = struct{}{}
		identity := cacheIdentity(authority, repository, digest)
		unlock := c.lockIdentity(identity)
		entries, _, err := c.store.List(ctx, "cache/objects/"+identity+"/representations/", "", 1000)
		if err != nil {
			unlock()
			return err
		}
		for _, entry := range entries {
			metadata, version, readErr := c.readMetadataEntry(ctx, entry.Key)
			if errors.Is(readErr, storage.ErrNotFound) {
				continue
			}
			if readErr != nil {
				unlock()
				return readErr
			}
			if metadata.State != "complete" || metadata.Authority != authority || metadata.Repository != repository || metadata.Digest != digest {
				continue
			}
			metadata.State = "cleanup_pending"
			metadata.CleanupAfter = cleanupAfter
			data, marshalErr := json.Marshal(metadata)
			if marshalErr != nil {
				unlock()
				return marshalErr
			}
			if _, err := c.store.Put(ctx, entry.Key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
				unlock()
				return err
			}
		}
		unlock()
	}
	return nil
}

func (c *Cache) MarkBlobCleanupPending(ctx context.Context, authority, repository, digest string) error {
	return c.MarkCleanupPending(ctx, authority, repository, []string{digest}, time.Now().UTC())
}
