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

func (c *Cache) put(ctx context.Context, authority, repository, digest, mediaType string, body io.Reader, size int64, beforeComplete func() error) error {
	if authority == "" || repository == "" || mediaType == "" || size < 0 {
		return errors.New("cache object identity or size is invalid")
	}
	identity := cacheIdentity(authority, repository, digest)
	dataKey := "cache/objects/" + identity + "/data"
	hasher, err := Hasher(digest)
	if err != nil {
		return err
	}
	source := &hashingReader{reader: body, hasher: hasher}
	entry, putErr := c.store.Put(ctx, dataKey, source, size, storage.PutOptions{IfAbsent: true})
	if putErr != nil && !errors.Is(putErr, storage.ErrAlreadyExists) {
		return putErr
	}
	if putErr == nil && source.count != size {
		_ = c.store.DeleteIfMatch(ctx, dataKey, entry.Version)
		return errors.New("cache object stream length mismatch")
	}
	if putErr == nil && digestValue(digest, source.hasher) != digest {
		_ = c.store.DeleteIfMatch(ctx, dataKey, entry.Version)
		return errors.Join(ErrDigestMismatch, errors.New("cache object digest mismatch"))
	}
	if errors.Is(putErr, storage.ErrAlreadyExists) {
		entry, err = c.store.Head(ctx, dataKey)
		if err != nil {
			return err
		}
		if entry.Size != size {
			return errors.New("existing cache object size mismatch")
		}
		if err := c.verifyData(ctx, dataKey, digest, size, entry.Version); err != nil {
			return err
		}
	}
	metadata := cacheMetadata{
		SchemaVersion: cacheSchemaCurrent, Authority: authority, Repository: repository, Digest: digest,
		MediaType: mediaType, Size: size, DataVersion: entry.Version,
		VerifiedAt: time.Now().UTC(), CleanupAfter: time.Now().UTC().Add(24 * time.Hour), State: "complete",
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
		if metadata.State == "cleanup_pending" {
			metadata.State = "complete"
			metadata.CleanupAfter = time.Now().UTC().Add(24 * time.Hour)
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

func (c *Cache) Get(ctx context.Context, authority, repository, digest, mediaType string) (CachedObject, error) {
	return c.get(ctx, authority, repository, digest, mediaType, false)
}

func (c *Cache) GetRetained(ctx context.Context, authority, repository, digest, mediaType string) (CachedObject, error) {
	return c.get(ctx, authority, repository, digest, mediaType, true)
}

func (c *Cache) get(ctx context.Context, authority, repository, digest, mediaType string, _ bool) (CachedObject, error) {
	identity := cacheIdentity(authority, repository, digest)
	unlock := c.lockIdentity(identity)
	defer unlock()
	metadata, err := c.readMetadata(ctx, cacheMetadataKey(identity, mediaType))
	if err != nil {
		return CachedObject{}, err
	}
	if metadata.SchemaVersion != cacheSchemaCurrent || metadata.Authority != authority || metadata.Repository != repository || metadata.Digest != digest || metadata.MediaType != mediaType {
		return CachedObject{}, storage.ErrCorruptObject
	}
	cleanupPending := metadata.State == "cleanup_pending"
	if metadata.State != "complete" && !cleanupPending {
		return CachedObject{}, storage.ErrCorruptObject
	}
	if err := c.verifyComplete(ctx, identity, metadata); err != nil {
		return CachedObject{}, err
	}
	dataKey := "cache/objects/" + identity + "/data"
	entry, err := c.store.Head(ctx, dataKey)
	if err != nil {
		return CachedObject{}, err
	}
	if entry.Version != metadata.DataVersion || entry.Size != metadata.Size {
		return CachedObject{}, errors.Join(storage.ErrCorruptObject, errors.New("cache data version does not match complete metadata"))
	}
	if err := c.verifyData(ctx, dataKey, digest, metadata.Size, metadata.DataVersion); err != nil {
		return CachedObject{}, err
	}
	readerCtx, cancel := context.WithCancel(ctx)
	pin, err := c.createReaderPin(ctx, identity)
	if err != nil {
		cancel()
		return CachedObject{}, err
	}
	object, err := c.store.Get(readerCtx, dataKey)
	if err != nil {
		cancel()
		_ = c.deleteReaderPin(context.Background(), pin)
		return CachedObject{}, err
	}
	body := newPinnedReader(c, object.Body, readerCtx, cancel, pin)
	return CachedObject{Authority: authority, Repository: repository, Digest: digest, MediaType: mediaType, Size: metadata.Size, DataVersion: metadata.DataVersion, Body: body}, nil
}

func (c *Cache) GetAny(ctx context.Context, authority, repository, digest string) (CachedObject, error) {
	return c.getAny(ctx, authority, repository, digest, false)
}

func (c *Cache) GetAnyRetained(ctx context.Context, authority, repository, digest string) (CachedObject, error) {
	return c.getAny(ctx, authority, repository, digest, true)
}
