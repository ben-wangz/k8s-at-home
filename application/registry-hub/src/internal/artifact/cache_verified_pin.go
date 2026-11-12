package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"slices"
	"time"
)

// VerifiedPin is an attempt-local proof; it must never be persisted or reused after Close.
type VerifiedPin struct {
	CachedRepresentation
	cache       *Cache
	reader      *pinnedReader
	fingerprint string
}

func (c *Cache) VerifyAnyPinned(ctx context.Context, authority, repository, digest string) (*VerifiedPin, error) {
	key := "cache/objects/" + cacheIdentity(authority, repository, digest) + "/data"
	before, err := storage.Fingerprint(ctx, c.store, key)
	if err != nil {
		return nil, err
	}
	object, err := c.getAnyMode(ctx, authority, repository, digest, false, true)
	if err != nil {
		return nil, err
	}
	proof := &VerifiedPin{CachedRepresentation: CachedRepresentation{Authority: authority, Repository: repository, Digest: digest, MediaType: object.MediaType, Size: object.Size, DataVersion: object.DataVersion, State: "complete"}, cache: c, reader: object.Body.(*pinnedReader), fingerprint: before}
	if err := proof.Check(ctx); err != nil {
		return nil, errors.Join(err, proof.Close())
	}
	return proof, nil
}

func (p *VerifiedPin) Check(ctx context.Context) error {
	if err := p.reader.checkPin(ctx); err != nil {
		return err
	}
	identity := cacheIdentity(p.Authority, p.Repository, p.Digest)
	index, _, err := p.cache.readRepresentationIndex(ctx, identity, p.Authority, p.Repository, p.Digest)
	if err != nil {
		return err
	}
	if !slices.Contains(index.MediaTypes, p.MediaType) {
		return storage.ErrConditionFailed
	}
	metadata, err := p.cache.readMetadata(ctx, cacheMetadataKey(identity, p.MediaType))
	if err != nil {
		return err
	}
	if metadata.DataVersion != p.DataVersion || metadata.Size != p.Size {
		return storage.ErrConditionFailed
	}
	if err := p.cache.verifyComplete(ctx, identity, metadata); err != nil {
		return err
	}
	key := "cache/objects/" + identity + "/data"
	fingerprint, err := storage.Fingerprint(ctx, p.cache.store, key)
	if err != nil {
		return err
	}
	if fingerprint != p.fingerprint {
		return storage.ErrConditionFailed
	}
	if fingerprint == "" {
		entry, err := p.cache.store.Head(ctx, key)
		if err != nil {
			return err
		}
		if entry.Version != p.DataVersion || entry.Size != p.Size {
			return storage.ErrConditionFailed
		}
	}
	return p.reader.pinState(ctx)
}

func (p *VerifiedPin) Close() error { return p.reader.Close() }

func (p *VerifiedPin) Context() context.Context { return p.reader.ctx }

func (r *pinnedReader) pinState(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(ctx.Err(), r.ctx.Err(), r.err)
}

func (r *pinnedReader) checkPin(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := errors.Join(ctx.Err(), r.ctx.Err(), r.err); err != nil {
		return err
	}
	if !r.pin.expires.After(time.Now()) {
		return storage.ErrLeaseLost
	}
	entry, err := r.cache.store.Head(ctx, r.pin.key)
	if errors.Is(err, storage.ErrNotFound) {
		return storage.ErrLeaseLost
	}
	if err == nil && entry.Version != r.pin.version {
		err = storage.ErrLeaseLost
	}
	return err
}
