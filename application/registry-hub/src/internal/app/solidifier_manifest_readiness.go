package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func (s *solidifier) prepareManifestBlobs(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest) (map[string]bool, map[string]tasks.BlobIntent, error) {
	ready := make(map[string]bool, len(manifest.blobs))
	intents := make(map[string]tasks.BlobIntent, len(manifest.blobs))
	for _, blob := range manifest.blobs {
		cached, intent, err := s.prepareStagedBlob(ctx, task, taskLease, manifest, blob)
		if err != nil {
			return nil, nil, err
		}
		if intent.CacheVersion != "" && intent.ActualSize != blob.size {
			return nil, nil, permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
		}
		ready[blob.digest], intents[blob.digest] = cached, intent
	}
	return ready, intents, nil
}

func (s *solidifier) manifestCanProgress(ctx context.Context, task *tasks.Task, manifest *stagedManifest, cached map[string]bool) (bool, error) {
	for _, ready := range cached {
		if ready {
			return true, nil
		}
	}
	if _, available := s.stagedManifestObservationEpoch(); !available {
		// Let normal admission yield before any Distribution preflight while GC is active.
		return true, nil
	}
	for _, blob := range manifest.blobs {
		if err := s.service.inventoryStart(ctx, task, manifest.ref.digest); err != nil {
			return false, err
		}
		present, err := s.distributionBlobPresent(ctx, task, blob, "")
		if err != nil || !present {
			return false, err
		}
	}
	// This preflight only avoids empty admissions; publication rechecks under protection.
	return true, nil
}
