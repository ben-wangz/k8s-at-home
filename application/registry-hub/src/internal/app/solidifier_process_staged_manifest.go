package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) processStagedManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, graphBytes int64, manifest *stagedManifest, childrenReady bool) (bool, error) {
	if !childrenReady {
		return false, nil
	}
	maxSessions, maxBytes := protectionLimits(s.cfg)
	if manifest.scopeBytes > maxBytes {
		return false, permanent("capacity_limit", errors.New("manifest scope exceeds the unanchored Distribution byte limit"))
	}
	cacheReadyByDigest, intentByDigest, err := s.prepareManifestBlobs(ctx, task, taskLease, manifest)
	if err != nil {
		return false, err
	}
	canProgress, err := s.manifestCanProgress(ctx, task, manifest, cacheReadyByDigest)
	if err != nil {
		return false, err
	}
	if !canProgress {
		return false, nil
	}
	admitted, err := s.admitManifestScope(ctx, task, taskLease, manifest, graphBytes, maxSessions, maxBytes, false)
	if err != nil {
		return false, withSolidificationFailureStage(err, "manifest_scope_admission")
	}
	if !admitted {
		finalized, err := s.finalizeReusedAnchoredManifest(ctx, task, taskLease, manifest)
		if err != nil {
			return false, withSolidificationFailureStage(err, "anchored_scope_reuse")
		}
		if finalized {
			return true, nil
		}
		admitted, err = s.admitManifestScope(ctx, task, taskLease, manifest, graphBytes, maxSessions, maxBytes, true)
		if err != nil {
			return false, withSolidificationFailureStage(err, "manifest_scope_recovery")
		}
		if !admitted {
			finalized, err := s.finalizeReusedAnchoredManifest(ctx, task, taskLease, manifest)
			if err != nil {
				return false, withSolidificationFailureStage(err, "anchored_scope_reuse")
			}
			if !finalized {
				return false, tasks.WaitFor(tasks.WaitingDeps, "dependencies_unavailable")
			}
			return true, nil
		}
	}
	if err := s.checkGraphEpoch(ctx); err != nil {
		return false, err
	}
	ready := true
	for _, blob := range manifest.blobs {
		cacheReady := cacheReadyByDigest[blob.digest]
		intent := intentByDigest[blob.digest]
		if err := s.service.inventoryStart(ctx, task, manifest.ref.digest); err != nil {
			return false, err
		}
		present, err := s.distributionBlobPresent(ctx, task, blob, manifest.sessionID)
		if err != nil {
			return false, withSolidificationFailureStage(err, "blob_presence_check")
		}
		if present {
			if cacheReady {
				if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, blob.digest); err != nil {
					return false, temporary("storage", err)
				}
			}
			continue
		}
		if cacheReady {
			blobTask, _, enqueueErr := s.service.enqueueBlobIntent(ctx, intent)
			if enqueueErr != nil && !errors.Is(enqueueErr, storage.ErrConditionFailed) {
				if s.service.logger != nil {
					s.service.logger.Warn("cached blob task admission deferred", "task_id", task.ID, "error_class", "queue")
				}
			} else if blobTask.Status == tasks.WaitingDeps && blobTask.WaitReason == "manifest_scope_admission" || blobTask.Status == tasks.VerifiedLocal {
				_, _ = s.tasks.Wake(ctx, intent.ID)
			}
		}
		ready = false
	}
	if !ready {
		if err := s.releaseEmptyManifestScope(ctx, task, taskLease, manifest.sessionID); err != nil {
			return false, err
		}
		return false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
	err = s.ensureManifest(requestCtx, task, taskLease, manifest.ref, manifest.ref.digest, manifest.sessionID)
	cancel()
	if err != nil {
		return false, withSolidificationFailureStage(err, "manifest_publication", manifest.sessionID)
	}
	if err := s.recordStagedManifestObservation(ctx, task, manifest.ref); err != nil {
		return false, err
	}
	if err := s.markStagedManifestPublished(ctx, task, manifest); err != nil {
		return false, err
	}
	if manifest.ref.digest == task.RootDigest && !isDigestReference(task.Reference) {
		return true, nil
	}
	if err := s.anchorManifestScope(ctx, task, taskLease, manifest); err != nil {
		return false, withSolidificationFailureStage(err, "manifest_anchor")
	}
	if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, manifest.ref.digest); err != nil {
		return false, temporary("storage", err)
	}
	return true, nil
}
