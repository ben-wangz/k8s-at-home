package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *Cache) readMetadataEntry(ctx context.Context, key string) (cacheMetadata, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		object, err := c.store.Get(ctx, key)
		if err != nil {
			return cacheMetadata{}, "", err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return cacheMetadata{}, "", readErr
		}
		if closeErr != nil {
			return cacheMetadata{}, "", closeErr
		}
		if len(data) > 1<<20 {
			return cacheMetadata{}, object.Version, errors.Join(storage.ErrCorruptObject, errors.New("cache metadata exceeds size limit"))
		}
		var metadata cacheMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return cacheMetadata{}, object.Version, errors.Join(storage.ErrCorruptObject, fmt.Errorf("decode cache metadata: %w", err))
		}
		if !validCacheMetadataKey(key, metadata) {
			return cacheMetadata{}, object.Version, storage.ErrCorruptObject
		}
		switch metadata.SchemaVersion {
		case cachePayloadSchemaCurrent:
			return metadata, object.Version, nil
		case cachePayloadSchemaPrevious, cacheSchemaPrevious:
			if metadata.IntegrityState == "" {
				metadata.IntegrityState = "legacy_unknown"
			}
			metadata.SchemaVersion = cachePayloadSchemaCurrent
			updated, marshalErr := json.Marshal(metadata)
			if marshalErr != nil {
				return cacheMetadata{}, "", marshalErr
			}
			entry, putErr := c.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return cacheMetadata{}, "", putErr
			}
			return metadata, entry.Version, nil
		default:
			return cacheMetadata{}, object.Version, storage.ErrCorruptObject
		}
	}
	return cacheMetadata{}, "", storage.ErrUnavailable
}

func validCacheMetadataKey(key string, metadata cacheMetadata) bool {
	identityEnd := strings.Index(key, "/representations/")
	if identityEnd < len("cache/objects/") || metadata.Authority == "" || metadata.Repository == "" || metadata.Digest == "" || metadata.MediaType == "" || metadata.Size < 0 || metadata.DataVersion == "" || metadata.State != "complete" && metadata.State != "cleanup_pending" {
		return false
	}
	identity := strings.TrimPrefix(key[:identityEnd], "cache/objects/")
	if cacheIdentity(metadata.Authority, metadata.Repository, metadata.Digest) != identity || cacheMetadataKey(identity, metadata.MediaType) != key {
		return false
	}
	_, err := Hasher(metadata.Digest)
	return err == nil
}

func (c *Cache) ensureComplete(ctx context.Context, identity string, metadata cacheMetadata, dataVersion string) error {
	marker := cacheComplete{SchemaVersion: cachePayloadSchemaCurrent, Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest, MediaType: metadata.MediaType, Size: metadata.Size, DataVersion: dataVersion, CompletedAt: time.Now().UTC(), IntegrityState: metadata.IntegrityState, IntegrityDigest: metadata.IntegrityDigest, IntegritySize: metadata.IntegritySize, IntegrityMethod: metadata.IntegrityMethod, IntegrityVerifiedAt: metadata.IntegrityVerifiedAt, IntegrityBackendAlgorithm: metadata.IntegrityBackendAlgorithm, IntegrityBackendValue: metadata.IntegrityBackendValue, IntegrityBackendScope: metadata.IntegrityBackendScope}
	data, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	key := cacheCompleteKey(identity, metadata.MediaType)
	_, err = c.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) {
		if verifyErr := c.verifyComplete(ctx, identity, metadata); verifyErr == nil {
			marker, readErr := c.readComplete(ctx, identity, metadata.MediaType)
			if readErr != nil {
				return readErr
			}
			if validIntegrityProof(metadata, metadata.Digest) && !integrityProofMatches(metadata, marker, metadata.Digest) {
				return c.updateCompleteIntegrity(ctx, identity, metadata)
			}
			return nil
		}
		marker, readErr := c.readComplete(ctx, identity, metadata.MediaType)
		if readErr != nil || marker.DataVersion != metadata.DataVersion || marker.Size != metadata.Size {
			return errors.Join(storage.ErrCorruptObject, readErr)
		}
		if updateErr := c.updateCompleteIntegrity(ctx, identity, metadata); updateErr != nil {
			return updateErr
		}
		return c.verifyComplete(ctx, identity, metadata)
	}
	return err
}

func (c *Cache) verifyComplete(ctx context.Context, identity string, metadata cacheMetadata) error {
	marker, err := c.readComplete(ctx, identity, metadata.MediaType)
	if err != nil {
		return err
	}
	return verifyCompleteMarker(identity, metadata, marker)
}

func verifyCompleteMarker(identity string, metadata cacheMetadata, marker cacheComplete) error {
	if marker.SchemaVersion != cachePayloadSchemaCurrent || marker.Authority != metadata.Authority || marker.Repository != metadata.Repository || marker.Digest != metadata.Digest || marker.MediaType != metadata.MediaType || marker.Size != metadata.Size || marker.DataVersion != metadata.DataVersion {
		return errors.Join(storage.ErrCorruptObject, errors.New("cache complete marker identity is invalid"))
	}
	return nil
}

func integrityProofMatches(metadata cacheMetadata, marker cacheComplete, digest string) bool {
	return validIntegrityProof(metadata, digest) && marker.IntegrityState == metadata.IntegrityState && marker.IntegrityDigest == metadata.IntegrityDigest && marker.IntegritySize == metadata.IntegritySize && marker.IntegrityMethod == metadata.IntegrityMethod && !marker.IntegrityVerifiedAt.IsZero() && marker.IntegrityVerifiedAt.Equal(metadata.IntegrityVerifiedAt) && marker.IntegrityBackendAlgorithm == metadata.IntegrityBackendAlgorithm && marker.IntegrityBackendValue == metadata.IntegrityBackendValue && marker.IntegrityBackendScope == metadata.IntegrityBackendScope
}

func (c *Cache) readComplete(ctx context.Context, identity, mediaType string) (cacheComplete, error) {
	return c.readCompleteKey(ctx, cacheCompleteKey(identity, mediaType))
}

func (c *Cache) readCompleteKey(ctx context.Context, key string) (cacheComplete, error) {
	for attempt := 0; attempt < 16; attempt++ {
		object, err := c.store.Get(ctx, key)
		if err != nil {
			return cacheComplete{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return cacheComplete{}, readErr
		}
		if closeErr != nil {
			return cacheComplete{}, closeErr
		}
		if len(data) > 1<<20 {
			return cacheComplete{}, errors.New("cache complete marker exceeds size limit")
		}
		var marker cacheComplete
		if err := json.Unmarshal(data, &marker); err != nil {
			return cacheComplete{}, fmt.Errorf("decode cache complete marker: %w", err)
		}
		if !validCacheCompleteKey(key, marker) {
			return cacheComplete{}, storage.ErrCorruptObject
		}
		switch marker.SchemaVersion {
		case cachePayloadSchemaCurrent:
			return marker, nil
		case cachePayloadSchemaPrevious, cacheSchemaPrevious:
			marker.SchemaVersion = cachePayloadSchemaCurrent
			updated, marshalErr := json.Marshal(marker)
			if marshalErr != nil {
				return cacheComplete{}, marshalErr
			}
			if _, putErr := c.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version}); errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			} else if putErr != nil {
				return cacheComplete{}, putErr
			}
		default:
			return cacheComplete{}, storage.ErrCorruptObject
		}
	}
	return cacheComplete{}, storage.ErrUnavailable
}

func validCacheCompleteKey(key string, marker cacheComplete) bool {
	identityEnd := strings.Index(key, "/complete/")
	if identityEnd < len("cache/objects/") || marker.Authority == "" || marker.Repository == "" || marker.Digest == "" || marker.MediaType == "" || marker.Size < 0 || marker.DataVersion == "" {
		return false
	}
	identity := strings.TrimPrefix(key[:identityEnd], "cache/objects/")
	if cacheIdentity(marker.Authority, marker.Repository, marker.Digest) != identity || cacheCompleteKey(identity, marker.MediaType) != key {
		return false
	}
	_, err := Hasher(marker.Digest)
	return err == nil
}

func (c *Cache) verifyData(ctx context.Context, key, digest string, size int64, version string) error {
	_, err := c.openVerifiedPayload(ctx, key, digest, size, version, false)
	return err
}

func cacheIdentity(authority, repository, digest string) string {
	hash := sha256.Sum256([]byte(authority + "\x00" + repository + "\x00" + digest))
	return hex.EncodeToString(hash[:])
}

func cacheMetadataKey(identity, mediaType string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(mediaType)))
	return "cache/objects/" + identity + "/representations/" + hex.EncodeToString(hash[:]) + ".json"
}

func cacheRepresentationIndexKey(identity string) string {
	return "cache/objects/" + identity + "/representation-index.json"
}

func cacheCompleteKey(identity, mediaType string) string {
	hash := sha256.Sum256([]byte(strings.TrimSpace(mediaType)))
	return "cache/objects/" + identity + "/complete/" + hex.EncodeToString(hash[:]) + ".json"
}

func digestValue(reference string, hasher hash.Hash) string {
	return storage.DigestFromHasher(reference, hasher)
}
