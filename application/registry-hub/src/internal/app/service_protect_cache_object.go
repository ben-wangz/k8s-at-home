package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) protectCacheObject(ctx context.Context, authority, repository, digest string) (bool, error) {
	if !s.cacheProtectionReady.Load() {
		return true, nil
	}
	active, err := s.tasks.Leases().Active(ctx, "objects/"+objectLeaseKey(authority, repository, digest))
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	intent, intentErr := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: authority, Repository: repository, Digest: digest})
	if intentErr != nil && !errors.Is(intentErr, storage.ErrNotFound) {
		return false, intentErr
	}
	if intentErr == nil {
		switch intent.State {
		case tasks.IntentWaitingCache, tasks.IntentWaitingQuota, tasks.IntentReady, tasks.IntentRunning, tasks.IntentRetryWait:
			return true, nil
		}
	}
	token := ""
	for {
		references, next, err := s.records.ListCacheProtectionReferences(ctx, digest, token, 500)
		if err != nil {
			return false, err
		}
		for _, reference := range references {
			if reference.Authority != authority || reference.Repository != repository {
				continue
			}
			task, err := s.tasks.Get(ctx, reference.TaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if task.Kind != tasks.RootKind {
				continue
			}
			if !activeTaskNeedsCache(task) && !task.GCProtected {
				continue
			}
			if task.RootDigest == digest && !task.RootPublished {
				return true, nil
			}
			if node, nodeErr := s.records.GetGraphNode(ctx, task.ID, tasks.GraphManifest, digest); nodeErr == nil {
				if node.LocalState != "anchored" {
					return true, nil
				}
				continue
			} else if !errors.Is(nodeErr, storage.ErrNotFound) {
				return false, nodeErr
			}
			if _, nodeErr := s.records.GetGraphNode(ctx, task.ID, tasks.GraphBlob, digest); nodeErr == nil {
				if intentErr != nil || intent.State != tasks.IntentVerified && intent.State != tasks.IntentAnchored {
					return true, nil
				}
				continue
			} else if !errors.Is(nodeErr, storage.ErrNotFound) {
				return false, nodeErr
			}
			if _, exists := task.Graph[digest]; exists && (intentErr != nil || intent.State != tasks.IntentVerified && intent.State != tasks.IntentAnchored) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func activeTaskNeedsCache(task tasks.Task) bool {
	switch task.Status {
	case tasks.Pending, tasks.Running, tasks.RetryWait, tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal, tasks.RepairPending:
		return true
	default:
		return false
	}
}

func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.BeginShutdown()
		s.tasks.Close()
		if s.gc != nil {
			s.gc.Close()
		}
		s.background.Wait()
		s.upstream.Close()
		s.registry.Close()
		_ = s.store.Close()
	})
}
