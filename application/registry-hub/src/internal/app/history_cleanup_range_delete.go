package app

import (
	"context"
	"errors"
	"strings"

	"registry-hub/internal/storage"
)

func historyCleanupCandidateEntry(ref historyCleanupCandidateRef) storage.Entry {
	return storage.Entry{Key: ref.Key, Version: ref.Version, MetadataFingerprint: ref.Fingerprint, ModTime: ref.ModTime, Size: ref.Size}
}

func historyCleanupReferenceChanged(ref historyCleanupCandidateRef, current storage.Entry) bool {
	if ref.Version != "" && current.Version != "" {
		return ref.Version != current.Version
	}
	if ref.Fingerprint != "" && current.MetadataFingerprint != "" {
		return ref.Fingerprint != current.MetadataFingerprint
	}
	return ref.ModTime != current.ModTime || ref.Size != current.Size
}

func (s *Service) deleteHistoryCleanupRangePrimary(ctx context.Context, category string, current storage.Entry, ref historyCleanupCandidateRef) (historyCleanupEntryProgress, error) {
	if category == "storage_snapshot_history" {
		return s.deleteHistoryCleanupSnapshotCandidate(ctx, current, ref)
	}
	if category == "full_work_receipts" {
		return s.deleteHistoryCleanupReceipt(ctx, current)
	}
	return s.deleteHistoryCleanupObject(ctx, current)
}

func (s *Service) saveHistoryCleanupRangePrimary(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress, ref historyCleanupCandidateRef, outcome historyCleanupEntryProgress) (historyCleanupProgress, bool, error) {
	category := operation.Categories[0]
	counts := progress.Counts[category]
	counts.Deleted += outcome.Deleted
	counts.DeletedBytes += outcome.DeletedBytes
	counts.Missing += outcome.Missing
	progress.Counts[category] = counts
	if ref.Index == nil && !strings.HasPrefix(ref.Key, terminalWorkHistoryPrefix) && !strings.HasPrefix(ref.Key, terminalTransferHistoryPrefix) &&
		!(category == "storage_snapshot_history" && ref.Manifest == nil) {
		return s.finishHistoryCleanupRangeCandidate(checkpointCtx, operation, progress)
	}
	progress.SegmentPrimarySettled = true
	if err := s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false); err != nil {
		return progress, false, err
	}
	return s.settleHistoryCleanupRangeTail(ctx, checkpointCtx, operation, progress, ref)
}

func (s *Service) settleHistoryCleanupRangeTail(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress, ref historyCleanupCandidateRef) (historyCleanupProgress, bool, error) {
	category := operation.Categories[0]
	if strings.HasPrefix(ref.Key, terminalWorkHistoryPrefix) || strings.HasPrefix(ref.Key, terminalTransferHistoryPrefix) {
		worker := terminalTransferHistoryRetentionWorker
		if strings.HasPrefix(ref.Key, terminalWorkHistoryPrefix) {
			worker = terminalWorkHistoryRetentionWorker
		}
		if err := s.releaseMissingTerminalHistoryBudget(ctx, worker, ref.Key, ref.Size); err != nil {
			return progress, false, err
		}
	}
	if category == "storage_snapshot_history" {
		if err := s.deleteHistoryCleanupSnapshotPlan(ctx, ref); err != nil {
			return progress, false, err
		}
	}
	if ref.Index != nil {
		var err error
		progress, err = s.settleHistoryCleanupRangeIndex(ctx, checkpointCtx, operation, progress, *ref.Index)
		if err != nil {
			return progress, false, err
		}
	}
	return s.finishHistoryCleanupRangeCandidate(checkpointCtx, operation, progress)
}

func (s *Service) settleHistoryCleanupRangeIndex(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress, index historyCleanupCandidateRef) (historyCleanupProgress, error) {
	category := operation.Categories[0]
	counts := progress.Counts[category]
	current, err := s.store.Head(ctx, index.Key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		if progress.SegmentIndexStarted {
			counts.Unconfirmed++
		} else {
			counts.Missing++
		}
	case err != nil:
		return progress, err
	default:
		if historyCleanupReferenceChanged(index, current) {
			err = storage.ErrConditionFailed
		} else {
			prefix := terminalTransferHistoryOrderPrefix
			if category == "terminal_work_history" {
				prefix = terminalWorkHistoryOrderPrefix
			}
			linked, linkErr := s.terminalHistoryIndexLinked(ctx, prefix, index.Key)
			if linkErr != nil {
				return progress, linkErr
			}
			if linked {
				err = storage.ErrConditionFailed
			} else {
				var outcome historyCleanupEntryProgress
				progress, outcome, err = s.removeHistoryCleanupRangeIndex(ctx, checkpointCtx, operation, progress, current)
				counts.Deleted += outcome.Deleted
				counts.DeletedBytes += outcome.DeletedBytes
				counts.Missing += outcome.Missing
			}
		}
		if errors.Is(err, storage.ErrConditionFailed) {
			counts.Protected++
			counts.ProtectedBytes += current.Size
			counts.Skipped++
			counts.SkipReasons["version_changed"]++
		} else if err != nil {
			return progress, err
		}
	}
	progress.Counts[category] = counts
	return progress, nil
}

func (s *Service) removeHistoryCleanupRangeIndex(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress, current storage.Entry) (historyCleanupProgress, historyCleanupEntryProgress, error) {
	if !progress.SegmentIndexStarted {
		progress.SegmentIndexStarted = true
		if err := s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false); err != nil {
			return progress, historyCleanupEntryProgress{}, err
		}
	}
	outcome, err := s.deleteHistoryCleanupObject(ctx, current)
	return progress, outcome, err
}

func (s *Service) finishHistoryCleanupRangeCandidate(ctx context.Context, operation historyCleanupOperation, progress historyCleanupProgress) (historyCleanupProgress, bool, error) {
	category := operation.Categories[0]
	counts := progress.Counts[category]
	counts.Scanned++
	progress.Counts[category] = counts
	progress.SegmentOffset++
	progress.SegmentStarted, progress.SegmentPrimarySettled, progress.SegmentIndexStarted = false, false, false
	return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
}
