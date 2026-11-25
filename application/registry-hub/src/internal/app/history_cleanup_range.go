package app

import (
	"context"
	"fmt"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

const (
	historyCleanupRangePrefix        = diagnostics.Prefix + "cleanup-range-previews/"
	historyCleanupSegmentPrefix      = diagnostics.Prefix + "cleanup-segments/"
	historyCleanupContinuationPrefix = diagnostics.Prefix + "cleanup-continuations/"
)

type historyCleanupRangeProgressRecord struct {
	SchemaVersion int                         `json:"schemaVersion"`
	PreviewID     string                      `json:"previewID"`
	Progress      historyCleanupRangeProgress `json:"progress"`
}

func historyCleanupRangePreviewKey(id string) string { return historyCleanupRangePrefix + id + ".json" }
func historyCleanupRangeProgressKey(id string) string {
	return historyCleanupRangePrefix + id + ".progress.json"
}
func historyCleanupSegmentKey(id string) string { return historyCleanupSegmentPrefix + id + ".json" }

func historyCleanupSegmentChunkKey(id string, ordinal int) string {
	return fmt.Sprintf("%s%s/chunks/%06d.json", historyCleanupSegmentPrefix, id, ordinal)
}

func historyCleanupContinuationKey(id string) string {
	return historyCleanupContinuationPrefix + id + ".json"
}

func (s *Service) createHistoryCleanupRangePreview(ctx context.Context, principal string, request historyCleanupRangeRequest) (historyCleanupRangePreview, error) {
	request, err := normalizeHistoryCleanupRangeRequest(request)
	if err != nil {
		return historyCleanupRangePreview{}, err
	}
	holder, err := lease.NewManager(s.store).Acquire(ctx, "diagnostics/history-cleanup-preview-api", requestID(), 30*time.Second)
	if err != nil {
		return historyCleanupRangePreview{}, err
	}
	defer lease.NewManager(s.store).Release(ctx, holder)
	if err := s.checkHistoryCleanupRangeCapacity(ctx); err != nil {
		return historyCleanupRangePreview{}, err
	}
	createdAt := time.Now().UTC()
	requested := historyCleanupRange{From: request.From.UTC(), To: request.To.UTC()}
	effective := s.effectiveHistoryCleanupRange(request.Category, requested, createdAt)
	progress := historyCleanupRangeProgress{Counts: newHistoryCleanupCounts(request.Category), SegmentID: diagnostics.ID("history-cleanup-segment", principal, requestID())}
	var continuation *historyCleanupRangeContinuation
	if request.Cursor != "" {
		value, _, readErr := inventory.Read[historyCleanupRangeContinuation](ctx, s.store, historyCleanupContinuationKey(request.Cursor))
		if readErr != nil {
			return historyCleanupRangePreview{}, readErr
		}
		if value.SchemaVersion != 1 || value.Principal != principal || value.Category != request.Category ||
			value.PolicyVersion != historyCleanupPolicyVersion || !value.RequestedRange.From.Equal(requested.From) ||
			!value.RequestedRange.To.Equal(requested.To) || !value.ExpiresAt.After(createdAt) {
			return historyCleanupRangePreview{}, storage.ErrConditionFailed
		}
		if value.NextPreviewID != "" {
			if err := s.enqueueHistoryCleanupRangePreview(ctx, value.NextPreviewID, principal); err != nil {
				return historyCleanupRangePreview{}, err
			}
			return s.historyCleanupRangePreviewStatus(ctx, value.NextPreviewID, principal)
		}
		continuation = &value
		progress = value.Progress
		effective = value.EffectiveRange
		progress.Counts = newHistoryCleanupCounts(request.Category)
		progress.ChunkIndex, progress.Chunk, progress.SegmentItems, progress.SegmentBytes = 0, nil, 0, 0
		progress.SegmentObjects = 0
		progress.FinishedAt = time.Time{}
		progress.SegmentID = diagnostics.ID("history-cleanup-segment", principal, requestID())
	}
	id := diagnostics.ID("history-cleanup-range-preview", principal, requestID())
	preview := historyCleanupRangePreview{
		SchemaVersion: 1, ID: id, Principal: principal, Category: request.Category,
		RequestedRange: requested, EffectiveRange: effective, PolicyVersion: historyCleanupPolicyVersion,
		Status: "queued", Counts: progress.Counts, CreatedAt: createdAt,
		ExpiresAt: createdAt.Add(max(time.Hour, s.diagnosticsConfig().HistoryCleanupPreviewTTL)),
		StatusURL: "/api/v1/history/cleanup/previews/" + id,
		Limits:    currentHistoryCleanupRangeLimits(),
	}
	if err := putHistoryCleanupJSON(ctx, s.store, historyCleanupRangeProgressKey(id), historyCleanupRangeProgressRecord{SchemaVersion: 1, PreviewID: id, Progress: progress}, storage.PutOptions{IfAbsent: true}); err != nil {
		return historyCleanupRangePreview{}, err
	}
	if err := putHistoryCleanupJSON(ctx, s.store, historyCleanupRangePreviewKey(id), preview, storage.PutOptions{IfAbsent: true}); err != nil {
		return historyCleanupRangePreview{}, err
	}
	if continuation != nil {
		if err := inventory.Update(ctx, s.store, historyCleanupContinuationKey(request.Cursor), func(current *historyCleanupRangeContinuation) error {
			if current.SchemaVersion != 1 || current.Principal != principal || current.NextPreviewID != "" && current.NextPreviewID != id {
				return storage.ErrConditionFailed
			}
			current.NextPreviewID = id
			return nil
		}); err != nil {
			return historyCleanupRangePreview{}, err
		}
	}
	if err := s.enqueueHistoryCleanupRangePreview(ctx, id, principal); err != nil {
		return historyCleanupRangePreview{}, err
	}
	return preview, nil
}

func (s *Service) enqueueHistoryCleanupRangePreview(ctx context.Context, id, principal string) error {
	preview, _, err := inventory.Read[historyCleanupRangePreview](ctx, s.store, historyCleanupRangePreviewKey(id))
	if err != nil {
		return err
	}
	if preview.Principal != principal || preview.PolicyVersion != historyCleanupPolicyVersion {
		return storage.ErrConditionFailed
	}
	if preview.Status == "completed" {
		return nil
	}
	payload := historyCleanupRangeJobPayload{PreviewID: id, Principal: principal, PolicyVersion: preview.PolicyVersion}
	job, err := s.backgroundQueue().Enqueue(ctx, historyCleanupPreviewKind, id, payload, preview.CreatedAt)
	if err != nil {
		return err
	}
	if job.ID != workqueue.ID(historyCleanupPreviewKind, id) {
		return storage.ErrCorruptObject
	}
	return nil
}

func normalizeHistoryCleanupRangeRequest(request historyCleanupRangeRequest) (historyCleanupRangeRequest, error) {
	if !historyCleanupCategoryAllowed(request.Category) || !historyCleanupUTC(request.From) || !historyCleanupUTC(request.To) ||
		!request.From.Before(request.To) || request.To.After(time.Now().UTC()) || request.To.Sub(request.From) > historyCleanupMaxRange ||
		request.Cursor != "" && !inventory.ValidID(request.Cursor) {
		return historyCleanupRangeRequest{}, storage.ErrCorruptObject
	}
	request.From, request.To = request.From.UTC(), request.To.UTC()
	return request, nil
}

func historyCleanupCategoryAllowed(category string) bool {
	for _, candidate := range historyCleanupCategories {
		if category == candidate {
			return true
		}
	}
	return false
}

func (s *Service) effectiveHistoryCleanupRange(category string, requested historyCleanupRange, now time.Time) historyCleanupRange {
	effective := requested
	if age := s.historyCleanupMinimumAge(category); age > 0 {
		boundary := now.Add(-age)
		if effective.To.After(boundary) {
			effective.To = boundary
		}
	} else if effective.To.After(now) {
		effective.To = now
	}
	if effective.To.Before(effective.From) {
		effective.To = effective.From
	}
	return effective
}

func (s *Service) historyCleanupMinimumAge(category string) time.Duration {
	cfg := s.diagnosticsConfig()
	switch category {
	case "diagnostic_events":
		return cfg.EventRetention
	case "terminal_work_history", "terminal_transfer_history":
		return cfg.TerminalHistoryRetention
	case "full_work_receipts":
		return cfg.FullWorkReceiptRetention
	case "diagnostic_collection_receipts":
		return cfg.CollectedReceiptRetention
	case "transfer_observations":
		return cfg.TransferObservationRetention
	default:
		return 0
	}
}

func newHistoryCleanupCounts(category string) historyCleanupCounts {
	return historyCleanupCounts{SkipReasons: map[string]int64{}, Coverage: historyCleanupCoverage(category)}
}

func currentHistoryCleanupRangeLimits() historyCleanupRangeLimits {
	return historyCleanupRangeLimits{
		MaxRangeSeconds: int64(historyCleanupMaxRange.Seconds()), MaxSegmentObjects: historyCleanupSegmentItems,
		MaxSegmentBytes: historyCleanupSegmentBytes, CheckpointObjects: historyCleanupBatchSize,
		StorageRequests: historyCleanupStorageRequests, MaxStepSeconds: int64(historyCleanupRangeBudget.Seconds()),
	}
}
