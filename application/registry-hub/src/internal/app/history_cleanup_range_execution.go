package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) historyCleanupSegmentStep(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress) (historyCleanupProgress, bool, error) {
	segment, _, err := inventory.Read[historyCleanupSegment](ctx, s.store, historyCleanupSegmentKey(operation.SegmentID))
	if err != nil {
		return progress, false, err
	}
	if segment.ID != operation.SegmentID || segment.PreviewID != operation.PreviewID ||
		segment.Category != operation.Categories[0] || segment.Items <= 0 || segment.Objects < segment.Items || segment.Objects > historyCleanupSegmentItems ||
		segment.Bytes < 0 || segment.Bytes > historyCleanupSegmentBytes || segment.Chunks <= 0 ||
		segment.Chunks != (segment.Items+historyCleanupBatchSize-1)/historyCleanupBatchSize {
		return progress, false, storage.ErrCorruptObject
	}
	if progress.SegmentOffset < 0 || progress.SegmentOffset > segment.Items {
		return progress, false, storage.ErrCorruptObject
	}
	if progress.SegmentOffset == segment.Items {
		progress.CategoryIndex = len(operation.Categories)
		if err := s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, true); err != nil {
			return progress, false, err
		}
		return progress, true, nil
	}
	chunkIndex := progress.SegmentOffset / historyCleanupBatchSize
	chunk, _, err := inventory.Read[historyCleanupSegmentChunk](ctx, s.store, historyCleanupSegmentChunkKey(segment.ID, chunkIndex))
	if err != nil {
		return progress, false, err
	}
	if chunk.SegmentID != segment.ID || chunk.Ordinal != chunkIndex || len(chunk.Candidates) == 0 {
		return progress, false, storage.ErrCorruptObject
	}
	expectedCandidates := min(historyCleanupBatchSize, segment.Items-chunkIndex*historyCleanupBatchSize)
	if len(chunk.Candidates) != expectedCandidates {
		return progress, false, storage.ErrCorruptObject
	}
	candidateIndex := progress.SegmentOffset % historyCleanupBatchSize
	if candidateIndex >= len(chunk.Candidates) {
		return progress, false, storage.ErrCorruptObject
	}
	ref := chunk.Candidates[candidateIndex]
	category := operation.Categories[0]
	scope := operation.Range
	if category == "diagnostic_events" {
		if progress.EventSealTo.IsZero() {
			scope = s.effectiveHistoryCleanupRange(category, scope, time.Now().UTC())
			progress.EventSealTo = scope.To
			progress.EventWatermarkAdvanced = false
		} else {
			scope.To = progress.EventSealTo
		}
		if !progress.EventWatermarkAdvanced {
			next, err := s.advanceHistoryCleanupEventSeal(ctx, checkpointCtx, operation, progress, scope)
			return next, false, err
		}
	}
	counts := progress.Counts[category]
	if progress.SegmentPrimarySettled {
		return s.settleHistoryCleanupRangeTail(ctx, checkpointCtx, operation, progress, ref)
	}
	current, headErr := s.store.Head(ctx, ref.Key)
	if errors.Is(headErr, storage.ErrNotFound) {
		outcome := historyCleanupEntryProgress{Missing: 1}
		if progress.SegmentStarted {
			counts.Unconfirmed++
			outcome.Missing = 0
		}
		progress.Counts[category] = counts
		return s.saveHistoryCleanupRangePrimary(ctx, checkpointCtx, operation, progress, ref, outcome)
	}
	if headErr != nil {
		return progress, false, headErr
	}
	if historyCleanupReferenceChanged(ref, current) {
		counts.Scanned++
		counts.Protected++
		counts.ProtectedBytes += current.Size
		counts.Skipped++
		counts.SkipReasons["version_changed"]++
		progress.Counts[category] = counts
		progress.SegmentOffset++
		progress.SegmentStarted = false
		return progress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
	}
	preview := historyCleanupRangePreview{ID: operation.PreviewID, Principal: operation.Principal, Category: category, EffectiveRange: scope, PolicyVersion: operation.PolicyVersion}
	indexEntry := current
	if ref.Index != nil {
		indexEntry, headErr = s.store.Head(ctx, ref.Index.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			counts.Scanned++
			counts.Protected++
			counts.ProtectedBytes += current.Size
			counts.Skipped++
			counts.SkipReasons["history_index_missing"]++
			progress.Counts[category] = counts
			progress.SegmentOffset++
			progress.SegmentStarted = false
			return progress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
		}
		if headErr != nil {
			return progress, false, headErr
		}
		if historyCleanupReferenceChanged(*ref.Index, indexEntry) {
			counts.Scanned++
			counts.Protected++
			counts.ProtectedBytes += current.Size
			counts.Skipped++
			counts.SkipReasons["version_changed"]++
			progress.Counts[category] = counts
			progress.SegmentOffset++
			progress.SegmentStarted = false
			return progress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
		}
	}
	candidate, inRange, err := s.recheckHistoryCleanupCandidate(ctx, preview, indexEntry, ref)
	if err != nil {
		return progress, false, err
	}
	if !inRange || !candidate.Eligible {
		reason := candidate.Reason
		if reason == "" {
			reason = "outside_range"
		}
		counts.Scanned++
		counts.Protected++
		counts.ProtectedBytes += current.Size
		counts.Skipped++
		counts.SkipReasons[reason]++
		progress.Counts[category] = counts
		progress.SegmentOffset++
		progress.SegmentStarted = false
		return progress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
	}
	counts.Eligible++
	counts.EligibleBytes += current.Size
	if !progress.SegmentStarted {
		progress.SegmentStarted = true
		if err := s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false); err != nil {
			return progress, false, err
		}
	}
	outcome, deleteErr := s.deleteHistoryCleanupRangePrimary(ctx, category, current, ref)
	conflicted := errors.Is(deleteErr, storage.ErrConditionFailed)
	if conflicted {
		counts.Protected++
		counts.ProtectedBytes += current.Size
		counts.SkipReasons["version_changed"]++
		outcome.Skipped++
		deleteErr = nil
	}
	if deleteErr != nil {
		return progress, false, deleteErr
	}
	progress.Counts[category] = counts
	if outcome.Deleted > 0 || outcome.Missing > 0 {
		return s.saveHistoryCleanupRangePrimary(ctx, checkpointCtx, operation, progress, ref, outcome)
	}
	counts.Scanned++
	counts.Skipped += outcome.Skipped
	if category == "storage_snapshot_history" && outcome.Skipped > 0 && !conflicted {
		counts.Protected++
		counts.ProtectedBytes += current.Size
		counts.SkipReasons["snapshot_children_remaining"]++
	}
	progress.Counts[category] = counts
	progress.SegmentOffset++
	progress.SegmentStarted = false
	return progress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
}

func (s *Service) advanceHistoryCleanupEventSeal(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress, scope historyCleanupRange) (historyCleanupProgress, error) {
	from := progress.EventSealedThrough
	if from.Before(scope.From) {
		from = scope.From
	}
	if from.Before(scope.To) {
		endOfDay := from.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		if endOfDay.After(scope.To) {
			endOfDay = scope.To
		}
		if err := diagnostics.SealEventHistoryDay(ctx, s.store, from, endOfDay); err != nil {
			return progress, err
		}
		progress.EventSealedThrough, progress.EventWatermarkAdvanced = endOfDay, false
		progress.EventWatermark = diagnostics.EventWatermarkProgress{}
	} else {
		next, done, err := diagnostics.AdvanceSealedEventHistoryWatermarkStep(ctx, s.store, scope.From, scope.To, progress.EventWatermark)
		if err != nil {
			return progress, err
		}
		progress.EventSealedThrough, progress.EventWatermarkAdvanced, progress.EventWatermark = from, done, next
	}
	return progress, s.saveHistoryCleanupOperation(checkpointCtx, operation, progress, false)
}
