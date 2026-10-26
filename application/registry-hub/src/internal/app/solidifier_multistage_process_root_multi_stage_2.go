package app

import (
	"context"
	"errors"
	"reflect"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) processRootMultiStage(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if task.RootDigest == "" || task.MediaType == "" {
		return permanent("task_state", errors.New("root task is missing its discovered manifest identity"))
	}
	graph := &stagedGraph{byManifest: make(map[string]*stagedManifest), blobs: make(map[string]contentRef), active: make(map[string]bool)}
	root := contentRef{digest: task.RootDigest, mediaType: task.MediaType, size: -1}
	rootManifest, err := s.discoverStagedManifest(ctx, task, taskLease, graph, root, 0, true)
	if err != nil {
		return err
	}
	graph.root = rootManifest.ref
	scopeID, err := protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
	if err != nil {
		return permanent("task_state", err)
	}
	rootEntry := graphEntry(graph.root)
	previousRoot, hasPreviousRoot := task.Graph[task.RootDigest]
	progressed := task.ScopeID != scopeID || task.Discovery != "complete" || task.RequestedBytes != graph.bytes || !hasPreviousRoot || !reflect.DeepEqual(previousRoot, rootEntry)
	task.ScopeID = scopeID
	task.Discovery = "complete"
	task.RequestedBytes = graph.bytes
	if task.Graph == nil {
		task.Graph = make(map[string]tasks.GraphEntry)
	}
	task.Graph[task.RootDigest] = rootEntry
	if progressed {
		task.GraphVersion++
		if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
			return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
		}
	}
	if err := s.assignStagedBlobScopes(ctx, task, graph); err != nil {
		return err
	}
	manifestReady := make(map[string]bool, len(graph.manifests))
	missing := false
	for _, manifest := range graph.manifests {
		childrenReady := true
		for _, child := range manifest.children {
			if !manifestReady[child.digest] {
				childrenReady = false
				break
			}
		}
		if !childrenReady {
			missing = true
			continue
		}
		cacheReadyByDigest := make(map[string]bool, len(manifest.blobs))
		intentByDigest := make(map[string]tasks.BlobIntent, len(manifest.blobs))
		for _, blob := range manifest.blobs {
			cacheReady, intent, err := s.prepareStagedBlob(ctx, task, manifest, blob)
			if err != nil {
				return err
			}
			if intent.CacheVersion != "" && intent.ActualSize != blob.size {
				return permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
			}
			cacheReadyByDigest[blob.digest] = cacheReady
			intentByDigest[blob.digest] = intent
		}
		maxSessions, maxBytes := protectionLimits(s.cfg)
		if manifest.scopeBytes > maxBytes {
			return permanent("capacity_limit", errors.New("manifest scope exceeds the unanchored Distribution byte limit"))
		}
		admitted, err := s.admitManifestScope(ctx, task, taskLease, manifest, graph.bytes, maxSessions, maxBytes, false)
		if err != nil {
			return withSolidificationFailureStage(err, "manifest_scope_admission")
		}
		if !admitted {
			observationEpoch, canObserve := s.stagedManifestObservationEpoch()
			if _, _, err := s.readDistributionManifest(ctx, task, manifest.ref); err == nil {
				if canObserve && s.stagedManifestObservationEpochIsCurrent(observationEpoch) {
					if err := s.recordStagedManifestPresence(ctx, task, manifest.ref, observationEpoch); err != nil {
						return err
					}
				}
				if err := s.finalizeAnchoredManifestBlobs(ctx, task, manifest); err != nil {
					return err
				}
				manifestReady[manifest.ref.digest] = true
				continue
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			admitted, err = s.admitManifestScope(ctx, task, taskLease, manifest, graph.bytes, maxSessions, maxBytes, true)
			if err != nil {
				return withSolidificationFailureStage(err, "manifest_scope_recovery")
			}
			if !admitted {
				observationEpoch, canObserve := s.stagedManifestObservationEpoch()
				if _, _, err := s.readDistributionManifest(ctx, task, manifest.ref); err != nil {
					if errors.Is(err, storage.ErrNotFound) {
						return temporary("protection_state", errors.New("anchored manifest remained missing after protection recovery"))
					}
					return err
				}
				if canObserve && s.stagedManifestObservationEpochIsCurrent(observationEpoch) {
					if err := s.recordStagedManifestPresence(ctx, task, manifest.ref, observationEpoch); err != nil {
						return err
					}
				}
				if err := s.finalizeAnchoredManifestBlobs(ctx, task, manifest); err != nil {
					return err
				}
				manifestReady[manifest.ref.digest] = true
				continue
			}
		}
		ready := true
		for _, blob := range manifest.blobs {
			cacheReady := cacheReadyByDigest[blob.digest]
			intent := intentByDigest[blob.digest]
			present, err := s.distributionBlobPresent(ctx, task, blob, manifest.sessionID)
			if err != nil {
				return withSolidificationFailureStage(err, "blob_presence_check")
			}
			if present {
				if cacheReady {
					if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, blob.digest); err != nil {
						return temporary("storage", err)
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
			missing = true
			if err := s.releaseEmptyManifestScope(ctx, task, taskLease, manifest.sessionID); err != nil {
				return err
			}
			continue
		}
		previousSession := task.ProtectionSessionID
		task.ProtectionSessionID = manifest.sessionID
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
		err = s.ensureManifest(requestCtx, task, taskLease, manifest.ref, manifest.ref.digest)
		cancel()
		task.ProtectionSessionID = previousSession
		if err != nil {
			return withSolidificationFailureStage(err, "manifest_publication")
		}
		if err := s.recordStagedManifestObservation(ctx, task, manifest.ref); err != nil {
			return err
		}
		if err := s.markStagedManifestPublished(ctx, task, manifest); err != nil {
			return err
		}
		if manifest.ref.digest == task.RootDigest && !isDigestReference(task.Reference) {
			manifestReady[manifest.ref.digest] = true
			continue
		}
		if err := s.anchorManifestScope(ctx, task, taskLease, manifest); err != nil {
			return withSolidificationFailureStage(err, "manifest_anchor")
		}
		if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, manifest.ref.digest); err != nil {
			return temporary("storage", err)
		}
		manifestReady[manifest.ref.digest] = true
	}
	if missing || !manifestReady[task.RootDigest] {
		return tasks.WaitFor(tasks.WaitingDeps, "dependencies_unavailable")
	}
	if !isDigestReference(task.Reference) {
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
		previousSession := task.ProtectionSessionID
		if rootTaskHasProtection(task, rootManifest.sessionID) {
			task.ProtectionSessionID = rootManifest.sessionID
		} else {
			task.ProtectionSessionID = ""
		}
		err := s.publishRootTag(requestCtx, task, taskLease)
		task.ProtectionSessionID = previousSession
		cancel()
		if err != nil {
			return withSolidificationFailureStage(err, "root_tag_publication")
		}
		if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, task.RootDigest); err != nil {
			return temporary("storage", err)
		}
	}
	if rootTaskHasProtection(task, rootManifest.sessionID) {
		if err := s.anchorManifestScope(ctx, task, taskLease, rootManifest); err != nil {
			return withSolidificationFailureStage(err, "root_manifest_anchor")
		}
	}
	s.wakeOldestQuotaTask(ctx)
	return nil
}
