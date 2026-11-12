package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
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
	cached, err := s.inspectReadyBlob(ctx, s.cache, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		return err
	}
	if !cached.Present {
		return nil
	}
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		return err
	}
	intent, _, err := s.records.EnsureBlobIntentForReconciliation(ctx, identity, generation.Generation)
	if err != nil {
		return err
	}
	if intent.CancelRequested || intent.Generation != generation.Generation ||
		intent.State == tasks.IntentCancelled || intent.State == tasks.IntentFailed || intent.State == tasks.IntentAnchored {
		return nil
	}
	if err := s.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		return err
	}
	return s.activateBlobIntent(ctx, intent, artifact.CachedObject{Authority: identity.Authority, Repository: identity.Repository, Digest: identity.Digest, MediaType: cached.MediaType, Size: cached.Size, DataVersion: cached.DataVersion})
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
