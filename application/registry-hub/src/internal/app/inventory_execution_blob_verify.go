package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

func (s *Service) verifyBlobExecutionSourceLinks(ctx context.Context, job workqueue.Job, fact inventoryExecutionFact) (bool, error) {
	var progress inventoryExecutionProgress
	if len(job.Progress) == 0 || json.Unmarshal(job.Progress, &progress) != nil || !progress.Cursor.Done {
		return false, nil
	}
	demands, complete, err := s.executionDemands(ctx, fact)
	if err != nil || !complete || len(demands) == 0 {
		return false, err
	}
	expected := map[string]executionSourceLink{}
	for _, demand := range demands {
		links, complete, err := s.blobDemandExecutionLinks(ctx, job, fact, demand)
		if err != nil || !complete {
			return false, err
		}
		for _, link := range links {
			key := executionScopePrefix(link.ImageID, link.Digest, link.Generation) + job.ID + ".json"
			if previous, exists := expected[key]; exists && previous != link {
				return false, storage.ErrCorruptObject
			}
			expected[key] = link
		}
	}
	if len(expected) == 0 {
		return false, nil
	}
	for key, target := range expected {
		stored, _, err := inventory.Read[executionSourceLink](ctx, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if stored != target {
			return false, storage.ErrCorruptObject
		}
	}
	return true, nil
}

func (s *Service) executionDemands(ctx context.Context, fact inventoryExecutionFact) ([]tasks.BlobDemand, bool, error) {
	if fact.Demand != nil {
		if fact.Demand.IntentID != fact.TaskID || fact.Demand.RootTaskID == "" || fact.Demand.DemandID == "" {
			return nil, false, nil
		}
		return []tasks.BlobDemand{*fact.Demand}, true, nil
	}
	prefix := "solidification/blobs/owners/" + fact.TaskID + "/"
	token := ""
	demands := []tasks.BlobDemand{}
	for {
		entries, next, err := storage.Scan(ctx, s.store, prefix, token, 500)
		if err != nil {
			return nil, false, err
		}
		for _, entry := range entries {
			demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, entry.Key)
			if errors.Is(err, storage.ErrNotFound) {
				return nil, false, nil
			}
			if err != nil {
				return nil, false, err
			}
			if demand.SchemaVersion != 1 || demand.IntentID != fact.TaskID || demand.RootTaskID == "" || demand.DemandID == "" {
				return nil, false, storage.ErrCorruptObject
			}
			demands = append(demands, demand)
		}
		if next == "" {
			return demands, true, nil
		}
		token = next
	}
}

func (s *Service) blobDemandExecutionLinks(ctx context.Context, job workqueue.Job, fact inventoryExecutionFact, demand tasks.BlobDemand) ([]executionSourceLink, bool, error) {
	root, err := s.tasks.Get(ctx, demand.RootTaskID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if root.Authority != fact.Identity.Authority || root.Repository != fact.Identity.Repository {
		return nil, false, storage.ErrCorruptObject
	}
	at := job.CreatedAt
	if demand.CreatedAt.After(at) {
		at = demand.CreatedAt
	}
	links := []executionSourceLink{}
	matchedScope := false
	rootNode, err := s.records.GetGraphNode(ctx, root.ID, tasks.GraphManifest, root.RootDigest)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return nil, false, err
	}
	if err == nil && rootNode.ContentGeneration != nil {
		links = append(links, blobManifestExecutionLinks(job, fact, root, rootNode.Digest, *rootNode.ContentGeneration, true, at)...)
	}
	if demand.ScopeID == "" {
		return links, true, nil
	}
	token := ""
	for {
		nodes, next, err := s.records.ListGraphNodes(ctx, root.ID, token, 500)
		if err != nil {
			return nil, false, err
		}
		for _, node := range nodes {
			if node.Role != tasks.GraphManifest || node.ContentGeneration == nil {
				continue
			}
			scope, err := protectionScopeID(root.Authority, root.Repository, node.Digest, root.Generation)
			if err != nil {
				return nil, false, err
			}
			if scope == demand.ScopeID {
				matchedScope = true
				links = append(links, blobManifestExecutionLinks(job, fact, root, node.Digest, *node.ContentGeneration, false, at)...)
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if !matchedScope || len(links) == 0 {
		return nil, false, nil
	}
	return links, true, nil
}

func blobManifestExecutionLinks(job workqueue.Job, fact inventoryExecutionFact, root tasks.Task, digest string, generation uint64, includeReference bool, at time.Time) []executionSourceLink {
	images := []string{inventory.ID("platform", root.Authority, root.Repository, digest), inventoryIdentity(root.Authority, root.Repository, digest).ID()}
	if includeReference {
		images = append(images, inventoryIdentity(root.Authority, root.Repository, root.Reference).ID())
	}
	links := make([]executionSourceLink, 0, len(images))
	for _, imageID := range images {
		links = append(links, executionSourceLink{ImageID: imageID, Digest: digest, Generation: generation, WorkID: job.ID, TaskID: fact.TaskID, RootTaskID: root.ID, ObservedAt: at, SourceRevision: workqueue.SourceRevision(job)})
	}
	return links
}
