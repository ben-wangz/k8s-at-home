package app

import (
	"context"
	"errors"
	"reflect"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"time"
)

func (s *solidifier) processRootMultiStage(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if task.RootDigest == "" || task.MediaType == "" {
		return permanent("task_state", errors.New("root task is missing its discovered manifest identity"))
	}
	ctx, performance := withSolidificationPerformance(ctx)
	discoveryStarted := time.Now()
	graph, err := s.loadOrDiscoverGraph(ctx, task, taskLease)
	performance.discovery += time.Since(discoveryStarted)
	if err != nil {
		return err
	}
	performance.discoveryReused = graph.reused
	performance.manifestCount, performance.blobCount, performance.closureBytes = len(graph.manifests), len(graph.blobs), graph.bytes
	rootManifest := graph.byManifest[task.RootDigest]
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
	if err := s.assignStagedBlobScopes(ctx, task, taskLease, graph); err != nil {
		return err
	}
	selection, err := s.selectGraphWork(ctx, task, graph)
	if err != nil {
		return err
	}
	if selection.affected != nil {
		ctx = context.WithValue(ctx, graphEpochKey{}, selection.epoch)
	}
	manifestReady := make(map[string]bool, len(graph.manifests))
	missing := false
	for _, manifest := range graph.manifests {
		if err := s.checkGraphEpoch(ctx); err != nil {
			return err
		}
		childrenReady := true
		for _, child := range manifest.children {
			if !manifestReady[child.digest] {
				childrenReady = false
				break
			}
		}
		ready, skip := selection.skip(task, manifest)
		if !skip {
			ready, err = s.processStagedManifest(ctx, task, taskLease, graph.bytes, manifest, childrenReady)
			if err != nil {
				return err
			}
		}
		selection.ready[manifest.ref.digest] = ready
		manifestReady[manifest.ref.digest] = ready
		missing = missing || !ready
	}
	if err := s.checkGraphEpoch(ctx); err != nil {
		return err
	}
	if err := s.saveGraphExecution(ctx, task, taskLease, graph, selection); err != nil {
		return err
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
	s.wakeAvailableQuotaWaiters(ctx)
	return nil
}
