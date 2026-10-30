package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func (s *Service) indexBlobDemandRoot(ctx context.Context, job workqueue.Job, source inventoryExecutionFact, demand tasks.BlobDemand) error {
	if demand.RootTaskID == "" {
		return nil
	}
	root, err := s.tasks.Get(ctx, demand.RootTaskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	node, err := s.records.GetGraphNode(ctx, root.ID, tasks.GraphManifest, root.RootDigest)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil || node.ContentGeneration == nil {
		return err
	}
	return s.indexBlobExecutionManifest(ctx, job, source, root, demand, node.Digest, *node.ContentGeneration, true)
}

func (s *Service) indexBlobExecutionManifest(ctx context.Context, job workqueue.Job, source inventoryExecutionFact, root tasks.Task, demand tasks.BlobDemand, digest string, generation uint64, includeReference bool) error {
	if source.Kind != tasks.BlobKind || source.TaskID != demand.IntentID || source.Identity.Authority != root.Authority || source.Identity.Repository != root.Repository {
		return storage.ErrCorruptObject
	}
	at := job.CreatedAt
	if demand.CreatedAt.After(at) {
		at = demand.CreatedAt
	}
	images := []string{inventory.ID("platform", root.Authority, root.Repository, digest), inventoryIdentity(root.Authority, root.Repository, digest).ID()}
	if includeReference {
		images = append(images, inventoryIdentity(root.Authority, root.Repository, root.Reference).ID())
	}
	for _, imageID := range images {
		link := executionSourceLink{ImageID: imageID, Digest: digest, Generation: generation, WorkID: job.ID, TaskID: source.TaskID, RootTaskID: root.ID, ObservedAt: at}
		if err := s.saveExecutionLink(ctx, job, link); err != nil {
			return err
		}
	}
	return nil
}
