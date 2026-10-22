package artifact

import (
	"context"
	"errors"
	"strings"

	"registry-hub/internal/storage"
)

func (c *Cache) ListCompleteRepresentations(ctx context.Context, token string, limit int) ([]CachedRepresentation, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := c.store.List(ctx, "cache/objects/", token, limit)
	if err != nil {
		return nil, "", err
	}
	result := make([]CachedRepresentation, 0, len(entries))
	for _, entry := range entries {
		if !strings.Contains(entry.Key, "/representations/") || !strings.HasSuffix(entry.Key, ".json") {
			continue
		}
		metadata, _, err := c.readMetadataEntry(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if metadata.State != "complete" && metadata.State != "cleanup_pending" {
			continue
		}
		result = append(result, CachedRepresentation{
			Authority: metadata.Authority, Repository: metadata.Repository, Digest: metadata.Digest,
			MediaType: metadata.MediaType, Size: metadata.Size, DataVersion: metadata.DataVersion, State: metadata.State,
		})
	}
	return result, next, nil
}
