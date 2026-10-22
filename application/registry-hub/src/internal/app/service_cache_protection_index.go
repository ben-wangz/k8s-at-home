package app

import (
	"context"
	"registry-hub/internal/tasks"
)

func (s *Service) rebuildCacheProtectionIndex(ctx context.Context) error {
	if err := s.records.RebuildRootBlobDemandIndexes(ctx); err != nil {
		return err
	}
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 500)
		if err != nil {
			return err
		}
		for _, task := range items {
			if task.Kind != tasks.RootKind {
				continue
			}
			digests := make(map[string]struct{}, len(task.Graph)+1)
			if task.RootDigest != "" {
				digests[task.RootDigest] = struct{}{}
			}
			for digest, entry := range task.Graph {
				if digest != "" {
					digests[digest] = struct{}{}
				}
				if entry.Digest != "" {
					digests[entry.Digest] = struct{}{}
				}
			}
			graphToken := ""
			for {
				nodes, graphNext, err := s.records.ListGraphNodes(ctx, task.ID, graphToken, 500)
				if err != nil {
					return err
				}
				for _, node := range nodes {
					digests[node.Digest] = struct{}{}
				}
				if graphNext == "" {
					break
				}
				graphToken = graphNext
			}
			for digest := range digests {
				if err := s.records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, digest); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}
