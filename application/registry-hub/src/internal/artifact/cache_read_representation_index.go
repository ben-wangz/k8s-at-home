package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
)

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
