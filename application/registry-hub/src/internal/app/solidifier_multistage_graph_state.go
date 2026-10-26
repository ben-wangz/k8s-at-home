package app

import (
	"context"
	"errors"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) saveBlobGraphNodeState(ctx context.Context, task *tasks.Task, reference contentRef, state string) error {
	return s.saveGraphNodeState(ctx, task, reference, tasks.GraphBlob, true, state)
}

func (s *solidifier) saveGraphNodeState(ctx context.Context, task *tasks.Task, reference contentRef, role tasks.GraphRole, complete bool, state string) error {
	contentGeneration, err := s.contentGeneration(ctx, task.Authority, task.Repository, reference.digest)
	if err != nil {
		return withSolidificationFailureStage(temporary("generation", err), "task_progress_read")
	}
	epoch := s.gate.State().Epoch
	node := tasks.GraphNode{
		RootTaskID: task.ID, Role: role, Digest: reference.digest, MediaType: reference.mediaType,
		DeclaredSize: reference.size, ActualSize: reference.size, DiscoveryComplete: complete,
		LocalState: state, Generation: task.Generation, ContentGeneration: &contentGeneration, GCEpoch: epoch,
	}
	existing, err := s.service.records.GetGraphNode(ctx, task.ID, role, reference.digest)
	if err == nil {
		node = existing
		sameContent := existing.ContentGeneration != nil && *existing.ContentGeneration == contentGeneration && existing.GCEpoch == epoch
		if sameContent {
			if graphLocalStateRank(existing.LocalState) > graphLocalStateRank(state) {
				state = existing.LocalState
			}
			complete = complete || existing.DiscoveryComplete
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_read")
	}
	node.RootTaskID = task.ID
	node.Role = role
	node.Digest = reference.digest
	node.MediaType = reference.mediaType
	node.DeclaredSize = reference.size
	node.ActualSize = reference.size
	node.Generation = task.Generation
	node.ContentGeneration = &contentGeneration
	node.GCEpoch = epoch
	if graphLocalStateRank(state) >= graphLocalStateRank("verified_present") {
		_, err := s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, role, reference.digest, reference.size, contentGeneration, epoch)
		if errors.Is(err, storage.ErrNotFound) {
			state = "discovered"
		} else if err != nil {
			return withSolidificationFailureStage(temporary("task_state", err), "task_progress_read")
		}
	}
	node.LocalState = state
	node.DiscoveryComplete = complete
	if err := s.service.records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, reference.digest); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "protection_reconcile")
	}
	if err := s.service.records.SaveGraphNode(ctx, &node); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	return nil
}

func graphLocalStateRank(state string) int {
	switch state {
	case "discovering":
		return 0
	case "discovered":
		return 1
	case "verified_present":
		return 2
	case "published_unanchored":
		return 3
	case "anchored":
		return 4
	default:
		return -1
	}
}
