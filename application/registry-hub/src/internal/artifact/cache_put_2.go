package artifact

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"time"
)

func (c *Cache) put(ctx context.Context, authority, repository, digest, mediaType string, body io.Reader, size int64, beforeComplete func() error) error {
	if authority == "" || repository == "" || mediaType == "" || size < 0 {
		return errors.New("cache object identity or size is invalid")
	}
	identity := cacheIdentity(authority, repository, digest)
	dataKey := "cache/objects/" + identity + "/data"
	source, err := storage.NewIntegrityReader(ctx, body, digest, size, size)
	if err != nil {
		return err
	}
	started := time.Now()
	options := storage.VerifiedPutOptions{PutOptions: storage.PutOptions{IfAbsent: true}, Digest: digest}
	var entry storage.Entry
	var putErr error
	sourceFinalized := false
	if verified, ok := c.store.(storage.VerifiedPutter); ok {
		entry, putErr = verified.PutVerified(ctx, dataKey, source, size, options)
		if errors.Is(putErr, errors.ErrUnsupported) {
			if source.Count() != 0 {
				return errors.New("unsupported verified put consumed the source")
			}
			entry, sourceFinalized, putErr = c.putVerifiedFallback(ctx, identity, dataKey, digest, size, source, options.PutOptions)
		}
	} else {
		entry, sourceFinalized, putErr = c.putVerifiedFallback(ctx, identity, dataKey, digest, size, source, options.PutOptions)
	}
	storage.ReportIO(ctx, c.store, "hash", source.Count(), time.Since(started))
	if putErr != nil && !errors.Is(putErr, storage.ErrAlreadyExists) {
		if errors.Is(putErr, storage.ErrDigestMismatch) {
			return errors.Join(ErrDigestMismatch, putErr)
		}
		return putErr
	}
	if putErr == nil && !sourceFinalized {
		if finalizeErr := source.Finalize(); finalizeErr != nil {
			_ = c.store.DeleteIfMatch(ctx, dataKey, entry.Version)
			if errors.Is(finalizeErr, storage.ErrDigestMismatch) || errors.Is(finalizeErr, storage.ErrSizeExceeded) {
				return errors.Join(ErrDigestMismatch, finalizeErr)
			}
			return finalizeErr
		}
	}
	proofMethod := "write_stream_digest"
	if entry.ChecksumAlgorithm != "" && entry.ChecksumValue != "" && entry.ChecksumScope != "" {
		proofMethod = "write_stream_digest_with_backend_checksum"
	}
	proofAt := time.Now().UTC()
	if errors.Is(putErr, storage.ErrAlreadyExists) {
		entry, err = c.store.Head(ctx, dataKey)
		if err != nil {
			return err
		}
		if entry.Size != size {
			return errors.New("existing cache object size mismatch")
		}
		existing, inspectErr := c.InspectAny(ctx, authority, repository, digest, mediaType)
		if inspectErr != nil {
			return inspectErr
		}
		if existing.Present && existing.DataVersion == entry.Version && existing.Size == size {
			proofMethod, proofAt = existing.IntegrityMethod, existing.IntegrityVerifiedAt
		} else {
			if err := c.verifyData(ctx, dataKey, digest, size, entry.Version); err != nil {
				return err
			}
			proofMethod = "unknown_commit_readback"
		}
	}
	fingerprint, err := storage.Fingerprint(ctx, c.store, dataKey)
	if err != nil {
		return err
	}
	metadata := cacheMetadata{
		SchemaVersion: cachePayloadSchemaCurrent, Authority: authority, Repository: repository, Digest: digest,
		MediaType: mediaType, Size: size, DataVersion: entry.Version,
		DataFingerprint: fingerprint,
		VerifiedAt:      time.Now().UTC(), CleanupAfter: time.Now().UTC().Add(24 * time.Hour), State: "complete",
		IntegrityState: "verified", IntegrityDigest: digest, IntegritySize: size,
		IntegrityMethod: proofMethod, IntegrityVerifiedAt: proofAt,
		IntegrityBackendAlgorithm: entry.ChecksumAlgorithm, IntegrityBackendValue: entry.ChecksumValue, IntegrityBackendScope: entry.ChecksumScope,
		InventoryTracked: c.tracksInventory(),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	metaKey := cacheMetadataKey(identity, mediaType)
	metadataEntry, err := c.store.Put(ctx, metaKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) {
		var readErr error
		var version string
		metadata, version, readErr = c.readMetadataEntry(ctx, metaKey)
		if readErr != nil {
			return readErr
		}
		if metadata.Digest != digest || metadata.Size != size || metadata.MediaType != mediaType || metadata.DataVersion != entry.Version || metadata.State != "complete" && metadata.State != "cleanup_pending" {
			return errors.New("existing cache metadata does not match verified content")
		}
		if metadata.State == "cleanup_pending" || metadata.DataFingerprint != fingerprint || metadata.IntegrityState != "verified" || metadata.IntegrityDigest != digest || metadata.IntegritySize != size {
			metadata.DataFingerprint = fingerprint
			metadata.State = "complete"
			metadata.CleanupAfter = time.Now().UTC().Add(24 * time.Hour)
			metadata.IntegrityState = "verified"
			metadata.IntegrityDigest = digest
			metadata.IntegritySize = size
			metadata.IntegrityMethod = proofMethod
			metadata.IntegrityVerifiedAt = proofAt
			metadata.IntegrityBackendAlgorithm = entry.ChecksumAlgorithm
			metadata.IntegrityBackendValue = entry.ChecksumValue
			metadata.IntegrityBackendScope = entry.ChecksumScope
			updated, marshalErr := json.Marshal(metadata)
			if marshalErr != nil {
				return marshalErr
			}
			_, updateErr := c.store.Put(ctx, metaKey, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: version})
			if updateErr != nil {
				return updateErr
			}
		}
	} else if err != nil {
		return err
	}
	if metadataEntry.Version != "" {
		metadata.DataVersion = entry.Version
	}
	if beforeComplete != nil {
		if err := beforeComplete(); err != nil {
			return err
		}
	}
	if err := c.updateRepresentationIndex(ctx, authority, repository, digest, mediaType, true); err != nil {
		return err
	}
	return c.ensureComplete(ctx, identity, metadata, entry.Version)
}

func (c *Cache) putVerifiedFallback(ctx context.Context, identity, key, digest string, size int64, source *storage.IntegrityReader, options storage.PutOptions) (result storage.Entry, finalized bool, returnErr error) {
	stageCtx, cancel := context.WithCancel(ctx)
	pin, err := c.createStagingPin(stageCtx, identity)
	if err != nil {
		cancel()
		return storage.Entry{}, false, err
	}
	guard := newPinnedReader(c, nil, stageCtx, cancel, pin)
	defer func() { returnErr = errors.Join(returnErr, guard.Close()) }()
	ctx = stageCtx
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return storage.Entry{}, false, err
	}
	stagingKey := "cache/objects/" + identity + "/staging/" + hex.EncodeToString(raw[:])
	staging, err := c.store.Put(ctx, stagingKey, source, size, storage.PutOptions{IfAbsent: true})
	if err != nil {
		return storage.Entry{}, false, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		cleanupErr := c.store.DeleteIfMatch(cleanupCtx, stagingKey, staging.Version)
		if !errors.Is(cleanupErr, storage.ErrNotFound) && !errors.Is(cleanupErr, storage.ErrConditionFailed) {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	if err := source.Finalize(); err != nil {
		return storage.Entry{}, true, fallbackIntegrityError(err)
	}
	object, err := storage.GetIfMatch(ctx, c.store, stagingKey, staging.Version)
	if err != nil {
		return storage.Entry{}, true, err
	}
	if object.Version != staging.Version || object.Size != size {
		_ = object.Body.Close()
		return storage.Entry{}, true, storage.ErrConditionFailed
	}
	verified, err := storage.NewIntegrityReader(ctx, object.Body, digest, size, size)
	if err != nil {
		_ = object.Body.Close()
		return storage.Entry{}, true, err
	}
	entry, putErr := c.store.Put(ctx, key, verified, size, options)
	closeErr := object.Body.Close()
	if putErr != nil {
		return storage.Entry{}, true, errors.Join(putErr, closeErr)
	}
	if err := verified.Finalize(); err != nil {
		deleteErr := c.store.DeleteIfMatch(ctx, key, entry.Version)
		return storage.Entry{}, true, errors.Join(fallbackIntegrityError(err), closeErr, deleteErr)
	}
	if closeErr != nil {
		deleteErr := c.store.DeleteIfMatch(ctx, key, entry.Version)
		return storage.Entry{}, true, errors.Join(closeErr, deleteErr)
	}
	return entry, true, nil
}

func fallbackIntegrityError(err error) error {
	if errors.Is(err, storage.ErrDigestMismatch) || errors.Is(err, storage.ErrSizeExceeded) {
		return errors.Join(ErrDigestMismatch, err)
	}
	return err
}

func (c *Cache) Get(ctx context.Context, authority, repository, digest, mediaType string) (CachedObject, error) {
	return c.get(ctx, authority, repository, digest, mediaType, false)
}

func (c *Cache) GetRetained(ctx context.Context, authority, repository, digest, mediaType string) (CachedObject, error) {
	return c.get(ctx, authority, repository, digest, mediaType, true)
}

func (c *Cache) get(ctx context.Context, authority, repository, digest, mediaType string, _ bool) (CachedObject, error) {
	return c.openVerified(ctx, authority, repository, digest, mediaType, true)
}

func (c *Cache) GetAny(ctx context.Context, authority, repository, digest string) (CachedObject, error) {
	return c.getAny(ctx, authority, repository, digest, false)
}

func (c *Cache) GetAnyRetained(ctx context.Context, authority, repository, digest string) (CachedObject, error) {
	return c.getAny(ctx, authority, repository, digest, true)
}
