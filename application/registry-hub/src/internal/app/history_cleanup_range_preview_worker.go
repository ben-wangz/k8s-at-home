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

func (s *Service) historyCleanupRangePreviewStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var payload historyCleanupRangeJobPayload
	if json.Unmarshal(job.Payload, &payload) != nil || payload.PreviewID == "" || payload.Principal == "" || payload.PolicyVersion != historyCleanupPolicyVersion {
		return nil, false, storage.ErrCorruptObject
	}
	preview, _, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, historyCleanupRangePreviewKey(payload.PreviewID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false, workqueue.ErrSourceMissing
	}
	if err != nil {
		return nil, false, err
	}
	if preview.SchemaVersion != 1 || preview.Principal != payload.Principal || preview.PolicyVersion != payload.PolicyVersion ||
		preview.ID != payload.PreviewID {
		return nil, false, storage.ErrCorruptObject
	}
	progress, err := s.readHistoryCleanupRangeProgress(ctx, preview.ID, job.Progress)
	if err != nil {
		return nil, false, err
	}
	if progress.Counts.SkipReasons == nil {
		progress.Counts.SkipReasons = map[string]int64{}
	}
	if progress.Counts.Coverage == "" {
		progress.Counts.Coverage = historyCleanupCoverage(preview.Category)
	}
	if preview.Status == "completed" {
		return progress, true, nil
	}
	if !preview.ExpiresAt.After(time.Now().UTC()) {
		return progress, true, nil
	}
	budgetCtx := withHistoryCleanupRequestBudget(ctx, historyCleanupStorageRequests)
	checkpointCtx := withHistoryCleanupCheckpointBudget(budgetCtx)
	var pending []string
	if preview.EffectiveRange.From.Before(preview.EffectiveRange.To) {
		pending, err = s.historyCleanupRangeBackfillPending(budgetCtx, preview.Category)
	}
	if err != nil {
		return progress, false, err
	}
	if len(pending) > 0 {
		if preview.Status == "waiting_for_backfill" && slices.Equal(preview.BackfillPending, pending) {
			return progress, false, nil
		}
		preview.Status, preview.BackfillPending, preview.Counts = "waiting_for_backfill", pending, progress.Counts
		if err := s.saveHistoryCleanupRangeProgress(checkpointCtx, preview, progress); err != nil {
			return progress, false, err
		}
		return progress, false, nil
	}
	preview.BackfillPending = nil
	preview.Status = "running"
	prefixes := historyCleanupRangePrefixes(preview.Category, preview.EffectiveRange)
	resumed, found, err := s.readHistoryCleanupRangeOrphanChunk(budgetCtx, progress.SegmentID, progress.ChunkIndex)
	if err != nil {
		return progress, false, err
	}
	if found {
		progress = resumed
		if err := s.saveHistoryCleanupRangeCheckpoint(checkpointCtx, preview.ID, progress); err != nil {
			return progress, false, err
		}
	}
	finished := !progress.FinishedAt.IsZero()
	if !finished {
		scanCtx, cancel := context.WithTimeout(budgetCtx, historyCleanupRangeBudget)
		finished, err = s.scanHistoryCleanupRangeBatch(scanCtx, preview, &progress, prefixes)
		if errors.Is(err, errHistoryCleanupRequestBudget) || errors.Is(scanCtx.Err(), context.DeadlineExceeded) {
			err = nil
		}
		cancel()
	}
	if err != nil {
		preview.Counts = progress.Counts
		return progress, false, errors.Join(err, s.saveHistoryCleanupRangeProgress(checkpointCtx, preview, progress))
	}
	if finished {
		if progress.FinishedAt.IsZero() {
			progress.FinishedAt = time.Now().UTC()
			if progress.Cursor.Done && !progress.Snapshot.Pending && progress.PrefixIndex < len(prefixes) {
				progress.PrefixIndex++
				progress.Cursor = inventory.Cursor{}
			}
		}
	}
	if len(progress.Chunk) == historyCleanupBatchSize || finished {
		if len(progress.Chunk) > 0 {
			resumed := progress
			resumed.ChunkIndex++
			resumed.Chunk = nil
			checkpoint := historyCleanupRangeCheckpointFromProgress(resumed)
			if err := s.persistHistoryCleanupSegmentChunk(checkpointCtx, progress.SegmentID, progress.ChunkIndex, progress.Chunk, checkpoint); err != nil {
				return progress, false, err
			}
			progress = resumed
		}
	}
	if finished {
		if err := s.saveHistoryCleanupRangeCheckpoint(checkpointCtx, preview.ID, progress); err != nil {
			return progress, false, err
		}
		if progress.SegmentItems > 0 {
			segment := historyCleanupSegment{ID: progress.SegmentID, PreviewID: preview.ID, Category: preview.Category, Items: progress.SegmentItems, Objects: progress.SegmentObjects, Bytes: progress.SegmentBytes, Chunks: progress.ChunkIndex}
			if err := putHistoryCleanupJSON(checkpointCtx, s.store, historyCleanupSegmentKey(segment.ID), segment, storage.PutOptions{IfAbsent: true}); err != nil && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
				return progress, false, err
			}
			preview.SegmentID, preview.SegmentItems, preview.SegmentBytes = segment.ID, segment.Objects, segment.Bytes
		}
		preview.Status, preview.CompletedAt = "completed", progress.FinishedAt
		preview.ExpiresAt = preview.CompletedAt.Add(s.diagnosticsConfig().HistoryCleanupPreviewTTL)
		if progress.PrefixIndex < len(prefixes) {
			cursorState, marshalErr := json.Marshal(struct {
				PrefixIndex int              `json:"prefixIndex"`
				Cursor      inventory.Cursor `json:"cursor"`
			}{progress.PrefixIndex, progress.Cursor})
			if marshalErr != nil {
				return progress, false, marshalErr
			}
			cursor := diagnostics.ID("history-cleanup-continuation", preview.ID, string(cursorState))
			continuation := historyCleanupRangeContinuation{SchemaVersion: 1, SourcePreviewID: preview.ID, Principal: preview.Principal, Category: preview.Category,
				RequestedRange: preview.RequestedRange, EffectiveRange: preview.EffectiveRange, PolicyVersion: preview.PolicyVersion,
				Progress: progress, ExpiresAt: preview.ExpiresAt}
			if err := s.putHistoryCleanupContinuation(checkpointCtx, cursor, continuation); err != nil {
				return progress, false, err
			}
			preview.NextCursor = cursor
		}
	}
	preview.Counts = progress.Counts
	if err := s.saveHistoryCleanupRangeProgress(checkpointCtx, preview, progress); err != nil {
		return progress, false, err
	}
	return progress, finished, nil
}
