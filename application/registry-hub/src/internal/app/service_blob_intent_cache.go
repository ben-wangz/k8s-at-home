package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) scheduleCachedBlobIntent(identity tasks.BlobIdentity) {
	select {
	case s.blobIntentEvents <- identity:
	default:
		s.cacheIntentRescan.Store(true)
	}
}

func (s *Service) processCachedBlobIntent(ctx context.Context, identity tasks.BlobIdentity) error {
	cached, err := s.cache.GetAnyRetained(ctx, identity.Authority, identity.Repository, identity.Digest)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer cached.Body.Close()
	intent, err := s.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil {
		return err
	}
	return s.activateBlobIntent(ctx, intent, cached)
}

func (s *Service) reconcileCachedBlobIntentPage(ctx context.Context, token string) (string, error) {
	items, next, err := s.cache.ListCompleteRepresentations(ctx, token, 500)
	if err != nil {
		return token, err
	}
	seen := make(map[tasks.BlobIdentity]struct{}, len(items))
	for _, item := range items {
		if isManifestRepresentation(item.MediaType) {
			continue
		}
		identity := tasks.BlobIdentity{Authority: item.Authority, Repository: item.Repository, Digest: item.Digest}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		_, intentErr := s.records.GetBlobIntentByIdentity(ctx, identity)
		if intentErr == nil {
			continue
		}
		if !errors.Is(intentErr, storage.ErrNotFound) {
			return token, intentErr
		}
		select {
		case s.blobIntentEvents <- identity:
		default:
			return token, nil
		}
	}
	return next, nil
}

func isManifestRepresentation(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json":
		return true
	default:
		return false
	}
}
