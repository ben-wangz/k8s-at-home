package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

func (c *Cache) getAny(ctx context.Context, authority, repository, digest string, _ bool) (CachedObject, error) {
	identity := cacheIdentity(authority, repository, digest)
	index, _, err := c.readRepresentationIndex(ctx, identity, authority, repository, digest)
	if err != nil {
		return CachedObject{}, err
	}
	for _, mediaType := range index.MediaTypes {
		metadataKey := cacheMetadataKey(identity, mediaType)
		metadata, metadataVersion, readErr := c.readMetadataEntry(ctx, metadataKey)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if errors.Is(readErr, storage.ErrCorruptObject) {
			if invalidateErr := c.invalidateCorruptRepresentation(ctx, authority, repository, digest, mediaType, metadataVersion); invalidateErr != nil {
				return CachedObject{}, errors.Join(readErr, invalidateErr)
			}
			continue
		}
		if readErr != nil {
			return CachedObject{}, readErr
		}
		if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Authority != authority || metadata.Repository != repository || metadata.Digest != digest || metadata.MediaType != mediaType {
			return CachedObject{}, storage.ErrCorruptObject
		}
		cleanupPending := metadata.State == "cleanup_pending"
		if metadata.State != "complete" && !cleanupPending {
			return CachedObject{}, storage.ErrCorruptObject
		}
		object, getErr := c.get(ctx, authority, repository, digest, mediaType, true)
		if errors.Is(getErr, storage.ErrNotFound) {
			return CachedObject{}, errors.Join(storage.ErrUnavailable, errors.New("cache representation index references an incomplete object"))
		}
		if errors.Is(getErr, storage.ErrCorruptObject) {
			if removeErr := c.Remove(ctx, authority, repository, digest, mediaType); removeErr != nil {
				return CachedObject{}, errors.Join(getErr, removeErr)
			}
			continue
		}
		return object, getErr
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
			if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Authority == "" || metadata.Repository == "" || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 || metadata.DataVersion == "" || cacheIdentity(metadata.Authority, metadata.Repository, metadata.Digest) != identity {
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

func (c *Cache) readRepresentationIndex(ctx context.Context, identity, authority, repository, digest string) (cacheRepresentationIndex, string, error) {
	key := cacheRepresentationIndexKey(identity)
	for attempt := 0; attempt < 16; attempt++ {
		object, err := c.store.Get(ctx, key)
		if err != nil {
			return cacheRepresentationIndex{}, "", err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, cacheRepresentationIndexMaxSize+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return cacheRepresentationIndex{}, "", readErr
		}
		if closeErr != nil {
			return cacheRepresentationIndex{}, "", closeErr
		}
		if len(data) > cacheRepresentationIndexMaxSize {
			return cacheRepresentationIndex{}, "", storage.ErrCorruptObject
		}
		var index cacheRepresentationIndex
		if err := json.Unmarshal(data, &index); err != nil {
			return cacheRepresentationIndex{}, "", errors.Join(storage.ErrCorruptObject, err)
		}
		if index.Authority != authority || index.Repository != repository || index.Digest != digest || len(index.MediaTypes) == 0 || len(index.MediaTypes) > cacheRepresentationIndexMaxMediaTypes {
			return cacheRepresentationIndex{}, "", storage.ErrCorruptObject
		}
		seen := make(map[string]struct{}, len(index.MediaTypes))
		for _, mediaType := range index.MediaTypes {
			if mediaType == "" || len(mediaType) > 1024 {
				return cacheRepresentationIndex{}, "", storage.ErrCorruptObject
			}
			if _, exists := seen[mediaType]; exists {
				return cacheRepresentationIndex{}, "", storage.ErrCorruptObject
			}
			seen[mediaType] = struct{}{}
		}
		switch index.SchemaVersion {
		case cacheSchemaCurrent:
			return index, object.Version, nil
		case cacheSchemaPrevious:
			index.SchemaVersion = cacheSchemaCurrent
			updated, marshalErr := json.Marshal(index)
			if marshalErr != nil {
				return cacheRepresentationIndex{}, "", marshalErr
			}
			entry, putErr := c.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return cacheRepresentationIndex{}, "", putErr
			}
			return index, entry.Version, nil
		default:
			return cacheRepresentationIndex{}, "", storage.ErrCorruptObject
		}
	}
	return cacheRepresentationIndex{}, "", storage.ErrUnavailable
}
