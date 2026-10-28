package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
)

type Inspection struct {
	CachedRepresentation
	Present          bool
	Unknown          bool
	Reason           string
	InventoryTracked bool
}

func inspectJSON[T any](ctx context.Context, store storage.Store, key string) (T, error) {
	var result T
	object, err := store.Get(ctx, key)
	if err != nil {
		return result, err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return result, err
	}
	if len(data) > 1<<20 {
		return result, storage.ErrCorruptObject
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return result, errors.Join(storage.ErrCorruptObject, err)
	}
	return result, nil
}

func (c *Cache) Inspect(ctx context.Context, authority, repository, digest, mediaType string) (Inspection, error) {
	identity := cacheIdentity(authority, repository, digest)
	key := cacheMetadataKey(identity, mediaType)
	metadata, err := inspectJSON[cacheMetadata](ctx, c.store, key)
	result := Inspection{CachedRepresentation: CachedRepresentation{Authority: authority, Repository: repository, Digest: digest, MediaType: mediaType}}
	if errors.Is(err, storage.ErrNotFound) {
		result.Reason = "cache_metadata_missing"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !validCacheMetadataKey(key, metadata) || metadata.SchemaVersion != cacheSchemaCurrent && metadata.SchemaVersion != cacheSchemaPrevious {
		return result, storage.ErrCorruptObject
	}
	result.Size, result.DataVersion, result.State = metadata.Size, metadata.DataVersion, metadata.State
	result.InventoryTracked = metadata.InventoryTracked
	marker, err := inspectJSON[cacheComplete](ctx, c.store, cacheCompleteKey(identity, mediaType))
	if errors.Is(err, storage.ErrNotFound) {
		result.Reason = "cache_marker_missing"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !validCacheCompleteKey(cacheCompleteKey(identity, mediaType), marker) || marker.SchemaVersion != cacheSchemaCurrent && marker.SchemaVersion != cacheSchemaPrevious || marker.DataVersion != metadata.DataVersion || marker.Size != metadata.Size {
		return result, storage.ErrCorruptObject
	}
	dataKey := "cache/objects/" + identity + "/data"
	var entry storage.Entry
	if source, ok := c.store.(storage.Fingerprinter); ok {
		var fingerprint string
		entry, fingerprint, err = source.Fingerprint(ctx, dataKey)
		if err == nil && fingerprint != "" {
			if metadata.DataFingerprint == "" {
				result.Unknown = true
				result.Reason = "cache_legacy_version_unverified"
				return result, nil
			}
			if fingerprint != metadata.DataFingerprint {
				result.Reason = "cache_version_mismatch"
				return result, nil
			}
			entry.Version = metadata.DataVersion
		} else if err == nil {
			entry, err = c.store.Head(ctx, dataKey)
		}
	} else {
		entry, err = c.store.Head(ctx, dataKey)
	}
	if errors.Is(err, storage.ErrNotFound) {
		result.Reason = "cache_payload_missing"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if entry.Version != metadata.DataVersion || entry.Size != metadata.Size {
		result.Reason = "cache_version_mismatch"
		return result, nil
	}
	if metadata.VerifiedAt.IsZero() {
		result.Unknown = true
		result.Reason = "cache_verification_missing"
		return result, nil
	}
	result.Present = true
	return result, nil
}

func (c *Cache) ReadManifestForInventory(ctx context.Context, representation CachedRepresentation, limit int64) ([]byte, error) {
	if limit < 1 || representation.Size > limit {
		return nil, storage.ErrCorruptObject
	}
	inspection, err := c.Inspect(ctx, representation.Authority, representation.Repository, representation.Digest, representation.MediaType)
	if err != nil {
		return nil, err
	}
	if !inspection.Present && !inspection.Unknown {
		return nil, storage.ErrNotFound
	}
	object, err := c.store.Get(ctx, "cache/objects/"+cacheIdentity(representation.Authority, representation.Repository, representation.Digest)+"/data")
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, limit+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || object.Version != inspection.DataVersion || int64(len(data)) != inspection.Size || VerifyDigest(representation.Digest, data) != nil {
		return nil, storage.ErrCorruptObject
	}
	return data, nil
}
