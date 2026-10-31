package app

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) wakeBlobDemandRoots(ctx context.Context, intentID string) {
	s.wakeBlobDemandRootsExcept(ctx, intentID, "")
}

func (s *Service) wakeBlobDemandRootsExcept(ctx context.Context, intentID, excludedRootID string) {
	if err := s.deferBlobNotification(ctx, intentID, excludedRootID); err != nil && s.logger != nil {
		s.logger.Warn("blob dependency notification deferred", "intent_id", intentID, "error_class", "storage")
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.wakeBlobDemandRootsImmediately(bounded, intentID, excludedRootID); err != nil && s.logger != nil {
		s.logger.Warn("immediate blob dependency wake deferred", "intent_id", intentID, "error_class", "storage")
	}
}

func (s *Service) wakeBlobDemandRootsImmediately(ctx context.Context, intentID, excludedRootID string) error {
	intent, err := s.records.GetBlobIntent(ctx, intentID)
	if err != nil {
		return err
	}
	var wakeErr error
	targets := make(map[string]struct{})
	wake := func(rootID string) {
		if rootID == "" || rootID == excludedRootID {
			return
		}
		if _, exists := targets[rootID]; exists {
			return
		}
		targets[rootID] = struct{}{}
		if err := s.wakeBlobDemandRoot(ctx, rootID); err != nil {
			wakeErr = errors.Join(wakeErr, err)
		}
	}
	token := ""
	for {
		demands, next, err := s.records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if errors.Is(err, storage.ErrNotFound) {
			break
		}
		if err != nil {
			return errors.Join(wakeErr, err)
		}
		for _, demand := range demands {
			wake(demand.RootTaskID)
		}
		if next == "" {
			break
		}
		token = next
	}
	token = ""
	for {
		references, next, err := s.records.ListCacheProtectionReferences(ctx, intent.Identity.Digest, token, 500)
		if err != nil {
			return errors.Join(wakeErr, err)
		}
		for _, reference := range references {
			if reference.Authority != intent.Identity.Authority || reference.Repository != intent.Identity.Repository {
				continue
			}
			wake(reference.TaskID)
		}
		if next == "" {
			return wakeErr
		}
		token = next
	}
}

func (s *Service) notifyBlobDemandRoots(ctx context.Context, intentID string) error {
	return s.deferBlobNotification(ctx, intentID, "")
}

func (s *Service) notifyBlobDemandRootsExcept(ctx context.Context, intentID, excludedRootID string) error {
	return s.deferBlobNotification(ctx, intentID, excludedRootID)
}

func (s *Service) wakeBlobDemandRoot(ctx context.Context, rootID string) error {
	root, err := s.tasks.Get(ctx, rootID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if root.Kind != tasks.RootKind || root.CancelRequested || root.Status != tasks.WaitingDeps && root.Status != tasks.Running {
		return nil
	}
	_, err = s.tasks.Wake(ctx, root.ID)
	return err
}
