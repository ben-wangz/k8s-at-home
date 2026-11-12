package artifact

import (
	"context"
	"io"
)

func (c *Cache) VerifyAny(ctx context.Context, authority, repository, digest string) (CachedRepresentation, error) {
	object, err := c.getAnyMode(ctx, authority, repository, digest, false, true)
	return verifiedRepresentation(object, err)
}

func (c *Cache) Verify(ctx context.Context, authority, repository, digest, mediaType string) (CachedRepresentation, error) {
	object, err := c.openVerifiedMode(ctx, authority, repository, digest, mediaType, false, true)
	return verifiedRepresentation(object, err)
}

func verifiedRepresentation(object CachedObject, err error) (CachedRepresentation, error) {
	if err != nil {
		return CachedRepresentation{}, err
	}
	err = object.Body.Close()
	return CachedRepresentation{Authority: object.Authority, Repository: object.Repository, Digest: object.Digest, MediaType: object.MediaType, Size: object.Size, DataVersion: object.DataVersion, State: "complete", IntegrityState: object.IntegrityState, IntegrityMethod: object.IntegrityMethod, IntegrityVerifiedAt: object.IntegrityVerifiedAt, IntegrityBackendAlgorithm: object.IntegrityBackendAlgorithm, IntegrityBackendValue: object.IntegrityBackendValue, IntegrityBackendScope: object.IntegrityBackendScope}, err
}

func (c *Cache) PutCheckedRepresentation(ctx context.Context, authority, repository, digest, mediaType string, body io.Reader, size int64, guard func() error) (CachedRepresentation, error) {
	identity := cacheIdentity(authority, repository, digest)
	unlock := c.lockIdentity(identity)
	defer unlock()
	if err := c.put(ctx, authority, repository, digest, mediaType, body, size, guard); err != nil {
		return CachedRepresentation{}, err
	}
	metadata, err := c.readMetadata(ctx, cacheMetadataKey(identity, mediaType))
	return CachedRepresentation{Authority: authority, Repository: repository, Digest: digest, MediaType: mediaType, Size: size, DataVersion: metadata.DataVersion, State: metadata.State, IntegrityState: metadata.IntegrityState, IntegrityMethod: metadata.IntegrityMethod, IntegrityVerifiedAt: metadata.IntegrityVerifiedAt, IntegrityBackendAlgorithm: metadata.IntegrityBackendAlgorithm, IntegrityBackendValue: metadata.IntegrityBackendValue, IntegrityBackendScope: metadata.IntegrityBackendScope}, err
}
