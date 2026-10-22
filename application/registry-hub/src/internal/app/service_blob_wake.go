package app

import (
	"context"
	"errors"

	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) wakeBlobDemandRoots(ctx context.Context, intentID string) {
	if err := s.notifyBlobDemandRoots(ctx, intentID); err != nil && s.logger != nil {
		s.logger.Warn("blob dependency notification deferred", "intent_id", intentID, "error_class", "storage")
	}
}

func (s *Service) notifyBlobDemandRoots(ctx context.Context, intentID string) error {
	intent, err := s.records.GetBlobIntent(ctx, intentID)
	if err != nil {
		return err
	}
	token := ""
	for {
		demands, next, err := s.records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if err != nil {
			return err
		}
		for _, demand := range demands {
			if demand.RootTaskID != "" {
				if err := s.wakeBlobDemandRoot(ctx, demand.RootTaskID); err != nil {
					return err
				}
			}
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
			return err
		}
		for _, reference := range references {
			if reference.Authority == intent.Identity.Authority && reference.Repository == intent.Identity.Repository {
				if err := s.wakeBlobDemandRoot(ctx, reference.TaskID); err != nil {
					return err
				}
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	if intent.WakeAcknowledged {
		return nil
	}
	intent.WakeAcknowledged = true
	return s.records.SaveBlobIntent(ctx, &intent)
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
