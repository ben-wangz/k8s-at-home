package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (c *Cache) RefreshVerification(ctx context.Context, expected CachedRepresentation, guard func(context.Context) error) error {
	identity := cacheIdentity(expected.Authority, expected.Repository, expected.Digest)
	key := "cache/objects/" + identity + "/data"
	before, err := storage.Fingerprint(ctx, c.store, key)
	if err != nil {
		return err
	}
	verified, err := c.Verify(ctx, expected.Authority, expected.Repository, expected.Digest, expected.MediaType)
	if err != nil {
		return err
	}
	if verified.DataVersion != expected.DataVersion || verified.Size != expected.Size {
		return storage.ErrConditionFailed
	}
	after, err := storage.Fingerprint(ctx, c.store, key)
	if err != nil {
		return err
	}
	if before != after {
		return storage.ErrConditionFailed
	}
	if after == "" {
		entry, err := c.store.Head(ctx, key)
		if err != nil {
			return err
		}
		if entry.Version != verified.DataVersion || entry.Size != verified.Size {
			return storage.ErrConditionFailed
		}
	}
	_, err = c.persistIntegrityProof(ctx, identity, cacheMetadata{Authority: verified.Authority, Repository: verified.Repository, Digest: verified.Digest, MediaType: verified.MediaType, Size: verified.Size, DataVersion: verified.DataVersion, DataFingerprint: after, VerifiedAt: time.Now().UTC()}, "legacy_readback", guard)
	return err
}

func (c *Cache) persistIntegrityProof(ctx context.Context, identity string, expected cacheMetadata, method string, guard func(context.Context) error) (cacheMetadata, error) {
	unlock := c.lockIdentity(identity)
	defer unlock()
	metaKey := cacheMetadataKey(identity, expected.MediaType)
	metadata, version, err := c.readMetadataEntry(ctx, metaKey)
	if err != nil {
		return cacheMetadata{}, err
	}
	if metadata.Authority != expected.Authority || metadata.Repository != expected.Repository || metadata.Digest != expected.Digest || metadata.MediaType != expected.MediaType || metadata.DataVersion != expected.DataVersion || metadata.Size != expected.Size {
		return cacheMetadata{}, storage.ErrConditionFailed
	}
	fingerprint, err := storage.Fingerprint(ctx, c.store, "cache/objects/"+identity+"/data")
	if err != nil {
		return cacheMetadata{}, err
	}
	if metadata.DataFingerprint == fingerprint && metadata.IntegrityState == "verified" && metadata.IntegrityDigest == metadata.Digest && metadata.IntegritySize == metadata.Size && !metadata.IntegrityVerifiedAt.IsZero() {
		marker, markerErr := c.readComplete(ctx, identity, metadata.MediaType)
		if markerErr != nil {
			return cacheMetadata{}, markerErr
		}
		if integrityProofMatches(metadata, marker, metadata.Digest) {
			return metadata, nil
		}
	}
	if fingerprint != "" && expected.DataFingerprint != "" && fingerprint != expected.DataFingerprint {
		return cacheMetadata{}, storage.ErrConditionFailed
	}
	if fingerprint == "" {
		entry, headErr := c.store.Head(ctx, "cache/objects/"+identity+"/data")
		if headErr != nil {
			return cacheMetadata{}, headErr
		}
		if entry.Version != expected.DataVersion || entry.Size != expected.Size {
			return cacheMetadata{}, storage.ErrConditionFailed
		}
	}
	now := time.Now().UTC()
	metadata.DataFingerprint, metadata.VerifiedAt = fingerprint, now
	metadata.IntegrityState, metadata.IntegrityDigest, metadata.IntegritySize = "verified", metadata.Digest, metadata.Size
	metadata.IntegrityMethod, metadata.IntegrityVerifiedAt = method, now
	if guard != nil {
		if err := guard(ctx); err != nil {
			return cacheMetadata{}, err
		}
	}
	if err := c.updateCompleteIntegrity(ctx, identity, metadata); err != nil {
		return cacheMetadata{}, err
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return cacheMetadata{}, err
	}
	if _, err := c.store.Put(ctx, metaKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); err != nil {
		return cacheMetadata{}, err
	}
	return metadata, nil
}

func (c *Cache) updateCompleteIntegrity(ctx context.Context, identity string, metadata cacheMetadata) error {
	key := cacheCompleteKey(identity, metadata.MediaType)
	for attempt := 0; attempt < 16; attempt++ {
		object, err := c.store.Get(ctx, key)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		if len(data) > 1<<20 {
			return storage.ErrCorruptObject
		}
		var marker cacheComplete
		if err := json.Unmarshal(data, &marker); err != nil {
			return errors.Join(storage.ErrCorruptObject, err)
		}
		if !validCacheCompleteKey(key, marker) || marker.DataVersion != metadata.DataVersion || marker.Size != metadata.Size {
			return storage.ErrCorruptObject
		}
		if marker.IntegrityState == metadata.IntegrityState && marker.IntegrityDigest == metadata.IntegrityDigest && marker.IntegritySize == metadata.IntegritySize && marker.IntegrityMethod == metadata.IntegrityMethod && marker.IntegrityVerifiedAt.Equal(metadata.IntegrityVerifiedAt) && marker.IntegrityBackendAlgorithm == metadata.IntegrityBackendAlgorithm && marker.IntegrityBackendValue == metadata.IntegrityBackendValue && marker.IntegrityBackendScope == metadata.IntegrityBackendScope && marker.SchemaVersion == cachePayloadSchemaCurrent {
			return nil
		}
		marker.IntegrityState = metadata.IntegrityState
		marker.IntegrityDigest = metadata.IntegrityDigest
		marker.IntegritySize = metadata.IntegritySize
		marker.IntegrityMethod = metadata.IntegrityMethod
		marker.IntegrityVerifiedAt = metadata.IntegrityVerifiedAt
		marker.IntegrityBackendAlgorithm = metadata.IntegrityBackendAlgorithm
		marker.IntegrityBackendValue = metadata.IntegrityBackendValue
		marker.IntegrityBackendScope = metadata.IntegrityBackendScope
		marker.SchemaVersion = cachePayloadSchemaCurrent
		updated, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		_, err = c.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}
