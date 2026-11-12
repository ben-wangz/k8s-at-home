package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (c *Cache) openVerified(ctx context.Context, authority, repository, digest, mediaType string, consume bool) (CachedObject, error) {
	return c.openVerifiedMode(ctx, authority, repository, digest, mediaType, consume, false)
}

func (c *Cache) openVerifiedMode(ctx context.Context, authority, repository, digest, mediaType string, consume, forceVerify bool) (CachedObject, error) {
	identity := cacheIdentity(authority, repository, digest)
	unlock := c.lockIdentity(identity)
	metadata, _, err := c.readMetadataEntry(ctx, cacheMetadataKey(identity, mediaType))
	if err != nil {
		err = withCacheReadFailureStage("representation_metadata_read", err)
	}
	if err == nil && (metadata.SchemaVersion != cachePayloadSchemaCurrent || metadata.Authority != authority || metadata.Repository != repository || metadata.Digest != digest || metadata.MediaType != mediaType || metadata.State != "complete" && metadata.State != "cleanup_pending") {
		err = withCacheReadFailureStage("representation_metadata_validate", storage.ErrCorruptObject)
	}
	var marker cacheComplete
	if err == nil {
		marker, err = c.readComplete(ctx, identity, mediaType)
		if err == nil {
			err = withCacheReadFailureStage("completion_marker_verify", verifyCompleteMarker(identity, metadata, marker))
		} else {
			err = withCacheReadFailureStage("completion_marker_verify", err)
		}
	}
	if err == nil && metadata.IntegrityState == "invalid" {
		err = withCacheReadFailureStage("integrity_state_validate", storage.ErrCorruptObject)
	}
	var pin cacheReaderPin
	if err == nil {
		pin, err = c.createReaderPin(ctx, identity)
		err = withCacheReadFailureStage("reader_pin_create", err)
	}
	unlock()
	if err != nil {
		return CachedObject{}, err
	}
	readerCtx, cancel := context.WithCancel(ctx)
	reader := newPinnedReader(c, nil, readerCtx, cancel, pin)
	var body io.ReadCloser
	proofMatches := integrityProofMatches(metadata, marker, digest)
	if !forceVerify && proofMatches {
		body, err = c.openTrustedPayload(readerCtx, "cache/objects/"+identity+"/data", metadata.Size, metadata.DataVersion, metadata.DataFingerprint, metadata.IntegrityBackendAlgorithm, metadata.IntegrityBackendValue, metadata.IntegrityBackendScope, consume)
		if errors.Is(err, errors.ErrUnsupported) {
			body, err = c.openVerifiedPayload(readerCtx, "cache/objects/"+identity+"/data", digest, metadata.Size, metadata.DataVersion, consume)
		}
	} else {
		body, err = c.openVerifiedPayload(readerCtx, "cache/objects/"+identity+"/data", digest, metadata.Size, metadata.DataVersion, consume)
	}
	if err != nil {
		return CachedObject{}, withCacheReadFailureStage("payload_verify_or_open", errors.Join(err, reader.Close()))
	}
	if !forceVerify && !proofMatches {
		metadata, err = c.persistIntegrityProof(ctx, identity, metadata, "legacy_readback", nil)
		if err != nil {
			if body != nil {
				_ = body.Close()
			}
			return CachedObject{}, withCacheReadFailureStage("legacy_integrity_upgrade", errors.Join(err, reader.Close()))
		}
	}
	if consume && body != nil {
		observerCtx := storage.WithIOObserver(ctx, func(_ context.Context, operation string, count int64, duration time.Duration) {
			storage.ReportIO(ctx, c.store, operation, count, duration)
		})
		body = storage.MeasureBody(observerCtx, body)
	}
	reader.body = body
	return CachedObject{Authority: authority, Repository: repository, Digest: digest, MediaType: mediaType, Size: metadata.Size, DataVersion: metadata.DataVersion, IntegrityState: metadata.IntegrityState, IntegrityMethod: metadata.IntegrityMethod, IntegrityVerifiedAt: metadata.IntegrityVerifiedAt, IntegrityBackendAlgorithm: metadata.IntegrityBackendAlgorithm, IntegrityBackendValue: metadata.IntegrityBackendValue, IntegrityBackendScope: metadata.IntegrityBackendScope, Body: reader}, nil
}

func (c *Cache) openTrustedPayload(ctx context.Context, key string, size int64, version, fingerprint, checksumAlgorithm, checksumValue, checksumScope string, consume bool) (io.ReadCloser, error) {
	if source, ok := c.store.(storage.PayloadOpener); ok {
		payload, err := source.OpenPayload(ctx, key)
		if !errors.Is(err, errors.ErrUnsupported) {
			if err != nil {
				return nil, err
			}
			if payload.Size != size || payload.Fingerprint == "" || fingerprint != "" && payload.Fingerprint != fingerprint {
				_ = payload.Body.Close()
				return nil, storage.ErrCorruptObject
			}
			if err := payload.Body.Check(ctx); err != nil {
				_ = payload.Body.Close()
				return nil, err
			}
			if !consume {
				return nil, payload.Body.Close()
			}
			return &payloadCheckReader{PayloadReader: payload.Body, ctx: ctx}, nil
		}
	}
	object, err := storage.GetIfMatch(ctx, c.store, key, version)
	if errors.Is(err, storage.ErrConditionFailed) {
		return nil, errors.Join(storage.ErrCorruptObject, err)
	}
	if err != nil {
		return nil, err
	}
	if object.Size != size || object.Version != version || checksumAlgorithm != "" && backendChecksumMismatch(checksumAlgorithm, checksumValue, checksumScope, object.ChecksumAlgorithm, object.ChecksumValue, object.ChecksumScope) {
		_ = object.Body.Close()
		return nil, storage.ErrCorruptObject
	}
	if !consume {
		return nil, object.Body.Close()
	}
	return object.Body, nil
}

type payloadCheckReader struct {
	storage.PayloadReader
	ctx context.Context
}

func (r *payloadCheckReader) Read(buffer []byte) (int, error) {
	n, err := r.PayloadReader.Read(buffer)
	if errors.Is(err, io.EOF) {
		if checkErr := r.PayloadReader.Check(r.ctx); checkErr != nil {
			return n, checkErr
		}
	}
	return n, err
}

func (c *Cache) openVerifiedPayload(ctx context.Context, key, digest string, size int64, version string, consume bool) (io.ReadCloser, error) {
	verifyCtx := storage.WithIOPurpose(ctx, "cache_verification")
	if storage.IOPurpose(ctx) == "cleanup_verification" {
		verifyCtx = ctx
	}
	if source, ok := c.store.(storage.PayloadOpener); ok {
		payload, err := source.OpenPayload(verifyCtx, key)
		if !errors.Is(err, errors.ErrUnsupported) {
			if err != nil {
				return nil, err
			}
			return c.verifySeekable(verifyCtx, payload, digest, size, version, consume)
		}
	}
	if err := c.verifyRemote(verifyCtx, key, digest, size, version); err != nil {
		return nil, err
	}
	if !consume {
		return nil, nil
	}
	object, err := storage.GetIfMatch(ctx, c.store, key, version)
	if errors.Is(err, storage.ErrConditionFailed) {
		return nil, errors.Join(storage.ErrCorruptObject, err)
	}
	if err != nil {
		return nil, err
	}
	if object.Size != size || object.Version != version {
		_ = object.Body.Close()
		return nil, storage.ErrCorruptObject
	}
	return object.Body, nil
}

func (c *Cache) verifySeekable(ctx context.Context, payload storage.Payload, digest string, size int64, version string, consume bool) (body io.ReadCloser, err error) {
	defer func() {
		if err != nil || !consume {
			err = errors.Join(err, payload.Body.Close())
		}
	}()
	if payload.Size != size {
		return nil, storage.ErrCorruptObject
	}
	hasher, err := Hasher(digest)
	if err != nil {
		return nil, err
	}
	versionHash := hasher
	var destination io.Writer = hasher
	if !strings.HasPrefix(digest, "sha256:") {
		versionHash = sha256.New()
		destination = io.MultiWriter(hasher, versionHash)
	}
	started := time.Now()
	count, err := io.Copy(destination, storage.ContextReader{Context: ctx, Reader: payload.Body})
	storage.ReportIO(ctx, c.store, "read", count, time.Since(started))
	hashed := count
	if !strings.HasPrefix(digest, "sha256:") {
		hashed += count
	}
	storage.ReportIO(ctx, c.store, "hash", hashed, time.Since(started))
	if err != nil {
		return nil, err
	}
	if count != size || digestValue(digest, hasher) != digest || hex.EncodeToString(versionHash.Sum(nil)) != version {
		return nil, storage.ErrCorruptObject
	}
	if err := payload.Body.Check(ctx); err != nil {
		return nil, err
	}
	if !consume {
		return nil, nil
	}
	if _, err := payload.Body.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return payload.Body, nil
}

func (c *Cache) verifyRemote(ctx context.Context, key, digest string, size int64, version string) (err error) {
	object, err := storage.GetIfMatch(ctx, c.store, key, version)
	if errors.Is(err, storage.ErrConditionFailed) {
		return errors.Join(storage.ErrCorruptObject, err)
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, object.Body.Close()) }()
	if object.Size != size || object.Version != version {
		return storage.ErrCorruptObject
	}
	hasher, err := Hasher(digest)
	if err != nil {
		return err
	}
	started := time.Now()
	count, err := io.Copy(hasher, storage.ContextReader{Context: ctx, Reader: object.Body})
	storage.ReportIO(ctx, c.store, "hash", count, time.Since(started))
	if err != nil {
		return err
	}
	if count != size || digestValue(digest, hasher) != digest {
		return storage.ErrCorruptObject
	}
	return nil
}
