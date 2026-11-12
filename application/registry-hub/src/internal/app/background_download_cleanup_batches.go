package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/download"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

const downloadCleanupAttemptBatches = 32

func (s *Service) cleanupDownloadBatches(ctx context.Context, job *workqueue.Job, source downloadCleanupWork, progress download.CleanupProgress, proof *artifact.VerifiedPin, generation generationRecord, guard func(context.Context) error) (any, bool, error) {
	for range downloadCleanupAttemptBatches {
		if err := guard(ctx); err != nil {
			return progress, false, err
		}
		current, err := s.observeGeneration(ctx, source.Authority, source.Repository, source.Digest)
		if err != nil {
			return progress, false, err
		}
		if current.Generation != generation.Generation || current.Tombstone != generation.Tombstone {
			return progress, false, storage.ErrConditionFailed
		}
		if proof != nil && progress.Phase < 2 {
			if err := proof.Check(ctx); err != nil {
				return progress, false, err
			}
			present, err := s.downloadManager().CleanupTicketCurrent(ctx, source.CleanupTicket)
			if err != nil || !present {
				if err == nil {
					err = storage.ErrConditionFailed
				}
				return progress, false, err
			}
		}
		done, err := s.downloadManager().CleanupCommittedStep(ctx, source.CleanupTicket, &progress, guard)
		if err != nil || done {
			return progress, done, err
		}
		if err := s.backgroundQueue().Checkpoint(ctx, job, progress); err != nil {
			return progress, false, err
		}
	}
	return progress, false, nil
}
