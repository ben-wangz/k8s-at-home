package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

type Inspection struct {
	CachedRepresentation
	Present            bool
	Unknown            bool
	Reason             string
	InventoryTracked   bool
	VerifiedAt         time.Time
	PayloadFingerprint string
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
	if !validCacheMetadataKey(key, metadata) || !validCachePayloadSchema(metadata.SchemaVersion) {
		return result, storage.ErrCorruptObject
	}
	result.Size, result.DataVersion, result.State = metadata.Size, metadata.DataVersion, metadata.State
	result.IntegrityState, result.IntegrityMethod, result.IntegrityVerifiedAt = metadata.IntegrityState, metadata.IntegrityMethod, metadata.IntegrityVerifiedAt
	result.IntegrityBackendAlgorithm, result.IntegrityBackendValue, result.IntegrityBackendScope = metadata.IntegrityBackendAlgorithm, metadata.IntegrityBackendValue, metadata.IntegrityBackendScope
	result.InventoryTracked = metadata.InventoryTracked
	result.VerifiedAt = metadata.VerifiedAt
	marker, err := inspectJSON[cacheComplete](ctx, c.store, cacheCompleteKey(identity, mediaType))
	if errors.Is(err, storage.ErrNotFound) {
		result.Reason = "cache_marker_missing"
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !validCacheCompleteKey(cacheCompleteKey(identity, mediaType), marker) || !validCachePayloadSchema(marker.SchemaVersion) || marker.DataVersion != metadata.DataVersion || marker.Size != metadata.Size || marker.Authority != metadata.Authority || marker.Repository != metadata.Repository || marker.Digest != metadata.Digest || marker.MediaType != metadata.MediaType {
		return result, storage.ErrCorruptObject
	}
	dataKey := "cache/objects/" + identity + "/data"
	var entry storage.Entry
	if source, ok := c.store.(storage.Fingerprinter); ok {
		var fingerprint string
		entry, fingerprint, err = source.Fingerprint(ctx, dataKey)
		result.PayloadFingerprint = fingerprint
		if err == nil && fingerprint != "" {
			if metadata.DataFingerprint == "" {
				result.Unknown = true
				result.Reason = "cache_legacy_version_unverified"
				return result, nil
			}
			if fingerprint != metadata.DataFingerprint {
				result.Unknown = true
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
		result.Unknown = true
		result.Reason = "cache_version_mismatch"
		return result, nil
	}
	if metadata.IntegrityBackendAlgorithm != "" && backendChecksumMismatch(metadata.IntegrityBackendAlgorithm, metadata.IntegrityBackendValue, metadata.IntegrityBackendScope, entry.ChecksumAlgorithm, entry.ChecksumValue, entry.ChecksumScope) {
		result.Unknown = true
		result.Reason = "cache_backend_checksum_unconfirmed"
		return result, nil
	}
	if metadata.VerifiedAt.IsZero() {
		result.Unknown = true
		result.Reason = "cache_verification_missing"
		return result, nil
	}
	if !integrityProofMatches(metadata, marker, digest) {
		result.Unknown = true
		result.Reason = "cache_write_integrity_unconfirmed"
		return result, nil
	}
	result.Present = true
	return result, nil
}

func backendChecksumMismatch(expectedAlgorithm, expectedValue, expectedScope, actualAlgorithm, actualValue, actualScope string) bool {
	if actualAlgorithm == "" && actualValue == "" && actualScope == "" {
		return false
	}
	return expectedAlgorithm != actualAlgorithm || expectedValue != actualValue || expectedScope != actualScope
}

func validIntegrityProof(metadata cacheMetadata, digest string) bool {
	if metadata.IntegrityState != "verified" || metadata.IntegrityDigest != digest || metadata.IntegritySize != metadata.Size || metadata.IntegrityVerifiedAt.IsZero() {
		return false
	}
	switch metadata.IntegrityMethod {
	case "write_stream_digest", "write_stream_digest_with_backend_checksum", "legacy_readback", "repair_readback", "unknown_commit_readback":
		return true
	default:
		return false
	}
}

func validCachePayloadSchema(version int) bool {
	return version == cachePayloadSchemaCurrent || version == cachePayloadSchemaPrevious || version == cacheSchemaPrevious
}

func (c *Cache) ReadManifestForInventory(ctx context.Context, representation CachedRepresentation, limit int64) ([]byte, error) {
	if limit < 1 || representation.Size > limit {
		return nil, storage.ErrCorruptObject
	}
	object, err := c.Get(ctx, representation.Authority, representation.Repository, representation.Digest, representation.MediaType)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, limit+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || int64(len(data)) != object.Size || representation.Size > 0 && representation.Size != object.Size || representation.DataVersion != "" && representation.DataVersion != object.DataVersion {
		return nil, storage.ErrCorruptObject
	}
	return data, nil
}
