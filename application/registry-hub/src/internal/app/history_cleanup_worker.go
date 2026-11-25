package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"time"
)

const historyCleanupOperationPrefix = diagnostics.Prefix + "cleanup-operations/"

func historyCleanupOperationKey(id string) string {
	return historyCleanupOperationPrefix + id + ".json"
}

func (s *Service) historyCleanupStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var payload historyCleanupJobPayload
	var progress historyCleanupProgress
	if json.Unmarshal(job.Payload, &payload) != nil || payload.PreviewID == "" || payload.Principal == "" ||
		len(job.Progress) > 0 && json.Unmarshal(job.Progress, &progress) != nil {
		return progress, false, storage.ErrCorruptObject
	}
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(job.ID))
	if errors.Is(err, storage.ErrNotFound) {
		return progress, false, workqueue.ErrSourceMissing
	}
	if err != nil {
		return progress, false, err
	}
	categories, categoryErr := normalizeHistoryCleanupCategories(operation.Categories)
	if categoryErr != nil || operation.SchemaVersion != 1 || operation.ID != job.ID || operation.PreviewID != payload.PreviewID ||
		operation.Principal != payload.Principal || operation.PolicyVersion != payload.PolicyVersion || operation.SegmentID != payload.SegmentID ||
		payload.PolicyVersion != historyCleanupPolicyVersion || !slices.Equal(categories, operation.Categories) ||
		operation.Cutoff.IsZero() || operation.Cutoff.After(time.Now().UTC()) {
		return progress, false, storage.ErrCorruptObject
	}
	if operation.Status == "completed" {
		return progress, true, nil
	}
	if operation.Counts != nil {
		progress.Counts = operation.Counts
	}
	if progress.Counts == nil {
		progress.Counts = map[string]historyCleanupCounts{}
	}
	for _, category := range operation.Categories {
		counts, ok := progress.Counts[category]
		if !ok {
			counts = newHistoryCleanupCounts(category)
		} else {
			if counts.SkipReasons == nil {
				counts.SkipReasons = map[string]int64{}
			}
			if counts.Coverage == "" {
				counts.Coverage = historyCleanupCoverage(category)
			}
		}
		progress.Counts[category] = counts
	}
	if operation.SegmentID != "" {
		progress.SegmentID, progress.SegmentOffset, progress.SegmentStarted = operation.SegmentID, operation.SegmentOffset, operation.SegmentStarted
		progress.SegmentPrimarySettled, progress.SegmentIndexStarted = operation.SegmentPrimarySettled, operation.SegmentIndexStarted
		progress.EventSealedThrough, progress.EventWatermarkAdvanced = operation.EventSealedThrough, operation.EventWatermarkAdvanced
		progress.EventSealTo = operation.EventSealTo
		progress.EventWatermark = operation.EventWatermark
		budgetCtx := withHistoryCleanupRequestBudget(ctx, historyCleanupStorageRequests)
		checkpointCtx := withHistoryCleanupCheckpointBudget(budgetCtx)
		stepCtx, cancel := context.WithTimeout(budgetCtx, historyCleanupRangeBudget)
		defer cancel()
		segmentProgress, done, err := s.advanceHistoryCleanupSegment(stepCtx, checkpointCtx, operation, progress)
		if errors.Is(err, errHistoryCleanupRequestBudget) || errors.Is(err, context.DeadlineExceeded) && errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
			return segmentProgress, false, s.saveHistoryCleanupOperation(checkpointCtx, operation, segmentProgress, false)
		}
		return segmentProgress, done, err
	}
	if progress.CategoryIndex >= len(operation.Categories) {
		if err := s.saveHistoryCleanupOperation(ctx, operation, progress, true); err != nil {
			return progress, false, err
		}
		return progress, true, nil
	}
	category := operation.Categories[progress.CategoryIndex]
	prefixes := historyCleanupPrefixes(category)
	if progress.PrefixIndex >= len(prefixes) {
		return progress, false, storage.ErrCorruptObject
	}
	if category == "diagnostic_events" && operation.Range.From.Before(operation.Range.To) && !progress.EventWatermarkAdvanced {
		if err := diagnostics.SealEventHistoryRange(ctx, s.store, operation.Range.From, operation.Range.To); err != nil {
			return progress, false, err
		}
		progress.EventWatermarkAdvanced = true
		return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
	}
	entry, ok, err := progress.Cursor.Peek(ctx, s.store, prefixes[progress.PrefixIndex], historyCleanupBatchSize)
	if err != nil {
		return progress, false, err
	}
	if !ok {
		if !progress.Cursor.Done {
			return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
		}
		progress.Cursor = inventory.Cursor{}
		if progress.PrefixIndex+1 < len(prefixes) {
			progress.PrefixIndex++
		} else {
			progress.CategoryIndex++
			progress.PrefixIndex = 0
			progress.EventWatermarkAdvanced = false
		}
		return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
	}
	current, err := s.store.Head(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		if err := s.releaseHistoryProjectionBudgetForCategory(ctx, category, entry.Key, entry.Size); err != nil {
			return progress, false, err
		}
		counts := progress.Counts[category]
		counts.Scanned++
		counts.Missing++
		progress.Counts[category] = counts
		progress.Cursor.Advance()
		return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
	}
	if err != nil {
		return progress, false, err
	}
	if historyCleanupEntryChanged(entry, current) {
		progress.Cursor.Entries[progress.Cursor.Offset] = current
		return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
	}
	entry = current
	if category == "diagnostic_events" {
		if date, valid := historyCleanupEventDate(entry.Key); valid && date < operation.Cutoff.UTC().Format("20060102") && date != progress.EventSealedDate {
			day, parseErr := time.Parse("20060102", date)
			if parseErr != nil {
				return progress, false, storage.ErrCorruptObject
			}
			if err := diagnostics.SealEventHistoryRange(ctx, s.store, day, day.Add(24*time.Hour)); err != nil {
				return progress, false, err
			}
			progress.EventSealedDate = date
			return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
		}
	}
	counts := progress.Counts[category]
	counts.Scanned++
	candidate, err := s.classifyHistoryCleanupEntry(ctx, category, entry, operation.Cutoff)
	if errors.Is(err, storage.ErrNotFound) {
		counts.Missing++
		progress.Counts[category] = counts
		progress.Cursor.Advance()
		return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
	}
	if err != nil {
		return progress, false, err
	}
	if !candidate.Eligible {
		counts.Protected++
		counts.ProtectedBytes += entry.Size
		counts.Skipped++
		counts.SkipReasons[candidate.Reason]++
	} else {
		counts.Eligible++
		counts.EligibleBytes += entry.Size
		outcome, err := s.deleteHistoryCleanupEntry(ctx, category, entry, operation.Cutoff)
		if errors.Is(err, storage.ErrConditionFailed) && category == "storage_snapshot_history" {
			counts.Protected++
			counts.ProtectedBytes += entry.Size
			counts.Skipped++
			counts.SkipReasons["version_changed"]++
			progress.Counts[category] = counts
			progress.Cursor.Advance()
			return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
		}
		if errors.Is(err, storage.ErrConditionFailed) {
			current, headErr := s.store.Head(ctx, entry.Key)
			if errors.Is(headErr, storage.ErrNotFound) {
				counts.Missing++
				progress.Counts[category] = counts
				progress.Cursor.Advance()
				return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
			}
			if headErr != nil {
				return progress, false, errors.Join(err, headErr)
			}
			if historyCleanupEntryChanged(entry, current) {
				counts.Scanned--
				counts.Eligible--
				counts.EligibleBytes -= entry.Size
				progress.Counts[category] = counts
				progress.Cursor.Entries[progress.Cursor.Offset] = current
				return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
			}
		}
		if err != nil {
			return progress, false, err
		}
		counts.Deleted += outcome.Deleted
		counts.DeletedBytes += outcome.DeletedBytes
		counts.Missing += outcome.Missing
		counts.Skipped += outcome.Skipped
	}
	progress.Counts[category] = counts
	progress.Cursor.Advance()
	return progress, false, s.saveHistoryCleanupOperation(ctx, operation, progress, false)
}

func (s *Service) advanceHistoryCleanupSegment(ctx, checkpointCtx context.Context, operation historyCleanupOperation, progress historyCleanupProgress) (historyCleanupProgress, bool, error) {
	for range historyCleanupRangeBatchSize {
		offset := progress.SegmentOffset
		next, done, err := s.historyCleanupSegmentStep(ctx, checkpointCtx, operation, progress)
		if err != nil || done || next.SegmentOffset == offset {
			return next, done, err
		}
		progress = next
	}
	return progress, false, nil
}
