package app

import (
	"context"
	"errors"

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
