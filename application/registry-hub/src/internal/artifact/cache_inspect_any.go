package artifact

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (c *Cache) InspectAny(ctx context.Context, authority, repository, digest, preferred string) (Inspection, error) {
	index, err := inspectJSON[cacheRepresentationIndex](ctx, c.store, cacheRepresentationIndexKey(cacheIdentity(authority, repository, digest)))
	if errors.Is(err, storage.ErrNotFound) {
		return c.Inspect(ctx, authority, repository, digest, preferred)
	}
	if err != nil {
		return Inspection{}, err
	}
	if index.SchemaVersion != cacheSchemaCurrent && index.SchemaVersion != cacheSchemaPrevious || index.Authority != authority || index.Repository != repository || index.Digest != digest || len(index.MediaTypes) > cacheRepresentationIndexMaxMediaTypes {
		return Inspection{}, storage.ErrCorruptObject
	}
	var result Inspection
	for _, mediaType := range index.MediaTypes {
		inspection, err := c.Inspect(ctx, authority, repository, digest, mediaType)
		if err != nil {
			return result, err
		}
		if inspection.Present {
			return inspection, nil
		}
		if result.Digest == "" || inspection.Unknown {
			result = inspection
		}
	}
	if result.Digest == "" {
		return c.Inspect(ctx, authority, repository, digest, preferred)
	}
	return result, nil
}
