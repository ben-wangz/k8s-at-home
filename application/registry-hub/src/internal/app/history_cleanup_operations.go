package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"time"
)

const historyCleanupIdempotencyPrefix = diagnostics.Prefix + "cleanup-idempotency/"

func historyCleanupRequestHash(previewID, segmentID string) string {
	if segmentID != "" {
		return diagnostics.ID("history-cleanup-segment-request", previewID, segmentID)
	}
	return diagnostics.ID("history-cleanup-request", previewID)
}

func (s *Service) submitHistoryCleanup(ctx context.Context, principal string, idempotencyKey string, request historyCleanupExecutionRequest) (historyCleanupTaskView, int, error) {
	if request.PreviewID == "" || !validHistoryCleanupIdempotencyKey(idempotencyKey) {
		return historyCleanupTaskView{}, http.StatusBadRequest, storage.ErrCorruptObject
	}
	requestHash := historyCleanupRequestHash(request.PreviewID, request.SegmentID)
	idempotencyID := diagnostics.ID("history-cleanup-idempotency", principal, idempotencyKey)
	idempotencyPath := historyCleanupIdempotencyPrefix + idempotencyID + ".json"
	holder, err := lease.NewManager(s.store).Acquire(ctx, "diagnostics/history-cleanup-api", requestID(), 30*time.Second)
	if err != nil {
		if errors.Is(err, lease.ErrHeld) {
			return historyCleanupTaskView{}, http.StatusConflict, err
		}
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	defer lease.NewManager(s.store).Release(ctx, holder)
	if view, found, status, err := s.readIdempotentHistoryCleanup(ctx, idempotencyPath, requestHash, principal); found || err != nil {
		return view, status, err
	}
	var categories []string
	var cutoff time.Time
	var scope historyCleanupRange
	policyVersion := historyCleanupPolicyVersion
	segmentID := ""
	if request.SegmentID != "" {
		preview, _, readErr := inventory.Read[historyCleanupRangePreview](ctx, s.store, historyCleanupRangePreviewKey(request.PreviewID))
		if errors.Is(readErr, storage.ErrNotFound) || readErr == nil && preview.Principal != principal {
			return historyCleanupTaskView{}, http.StatusNotFound, storage.ErrNotFound
		}
		if readErr != nil {
			return historyCleanupTaskView{}, http.StatusServiceUnavailable, readErr
		}
		if !preview.ExpiresAt.After(time.Now().UTC()) {
			return historyCleanupTaskView{}, http.StatusGone, inventory.ErrExpired
		}
		if preview.Status != "completed" || preview.SegmentID != request.SegmentID || preview.PolicyVersion != policyVersion {
			return historyCleanupTaskView{}, http.StatusConflict, storage.ErrConditionFailed
		}
		segment, _, readErr := inventory.Read[historyCleanupSegment](ctx, s.store, historyCleanupSegmentKey(request.SegmentID))
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return historyCleanupTaskView{}, http.StatusServiceUnavailable, readErr
		}
		if readErr != nil || segment.ID != request.SegmentID || segment.PreviewID != preview.ID || segment.Category != preview.Category ||
			segment.Objects != preview.SegmentItems || segment.Bytes != preview.SegmentBytes {
			return historyCleanupTaskView{}, http.StatusConflict, errors.Join(storage.ErrConditionFailed, readErr)
		}
		categories, cutoff, scope, segmentID = []string{preview.Category}, preview.EffectiveRange.To, preview.EffectiveRange, segment.ID
	} else {
		preview, _, readErr := inventory.Read[historyCleanupPreview](ctx, s.store, historyCleanupPreviewKey(request.PreviewID))
		if errors.Is(readErr, storage.ErrNotFound) || readErr == nil && preview.Principal != principal {
			return historyCleanupTaskView{}, http.StatusNotFound, storage.ErrNotFound
		}
		if readErr != nil {
			return historyCleanupTaskView{}, http.StatusServiceUnavailable, readErr
		}
		if !preview.ExpiresAt.After(time.Now().UTC()) {
			return historyCleanupTaskView{}, http.StatusGone, inventory.ErrExpired
		}
		if preview.PolicyVersion != policyVersion || !preview.Complete {
			return historyCleanupTaskView{}, http.StatusConflict, storage.ErrConditionFailed
		}
		categories, cutoff = preview.Categories, preview.Cutoff
	}
	if err := s.pruneHistoryCleanupOperations(ctx); err != nil {
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	payload := historyCleanupJobPayload{PreviewID: request.PreviewID, Principal: principal, PolicyVersion: policyVersion, SegmentID: segmentID}
	source := diagnostics.ID("history-cleanup-request", principal, idempotencyKey, requestHash)
	jobID := workqueue.ID(historyCleanupWorkKind, source)
	operation := historyCleanupOperation{
		SchemaVersion: 1, ID: jobID, PreviewID: request.PreviewID, Principal: principal, Categories: categories,
		Cutoff: cutoff, Range: scope, SegmentID: segmentID, PolicyVersion: policyVersion, Status: "pending", Counts: map[string]historyCleanupCounts{},
		QueueSource: source, CreatedAt: time.Now().UTC(), IdempotencyID: idempotencyID,
	}
	if err := s.createHistoryCleanupOperation(ctx, operation); err != nil {
		if errors.Is(err, errHistoryCleanupCapacity) {
			return historyCleanupTaskView{}, http.StatusTooManyRequests, err
		}
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrCorruptObject) {
			return historyCleanupTaskView{}, http.StatusConflict, err
		}
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	idempotency := historyCleanupIdempotency{SchemaVersion: 1, RequestID: requestHash, OperationID: jobID, ExpiresAt: operation.ExpiresAt}
	if err := s.createHistoryCleanupIdempotency(ctx, idempotencyPath, idempotency); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) {
			return historyCleanupTaskView{}, http.StatusConflict, err
		}
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	job, err := s.backgroundQueue().Enqueue(ctx, historyCleanupWorkKind, source, payload, operation.CreatedAt)
	if errors.Is(err, storage.ErrCorruptObject) {
		return historyCleanupTaskView{}, http.StatusConflict, err
	}
	if err != nil {
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	if job.ID != jobID {
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, storage.ErrCorruptObject
	}
	return historyCleanupViewFromJob(job, operation), http.StatusAccepted, nil
}

func (s *Service) createHistoryCleanupOperation(ctx context.Context, operation historyCleanupOperation) error {
	key := historyCleanupOperationKey(operation.ID)
	stored, _, readErr := inventory.Read[historyCleanupOperation](ctx, s.store, key)
	if readErr == nil {
		if stored.SchemaVersion != operation.SchemaVersion || stored.ID != operation.ID || stored.PreviewID != operation.PreviewID ||
			stored.Principal != operation.Principal || stored.PolicyVersion != operation.PolicyVersion || stored.IdempotencyID != operation.IdempotencyID ||
			stored.QueueSource != operation.QueueSource || !stored.Cutoff.Equal(operation.Cutoff) || stored.SegmentID != operation.SegmentID ||
			stored.Range != operation.Range || !slices.Equal(stored.Categories, operation.Categories) {
			return storage.ErrConditionFailed
		}
		return nil
	}
	if !errors.Is(readErr, storage.ErrNotFound) {
		return readErr
	}
	if err := s.ensureHistoryCleanupCapacity(ctx); err != nil {
		return err
	}
	err := putHistoryCleanupJSON(ctx, s.store, key, operation, storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	stored, _, readErr = inventory.Read[historyCleanupOperation](ctx, s.store, key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if stored.SchemaVersion != operation.SchemaVersion || stored.ID != operation.ID || stored.PreviewID != operation.PreviewID ||
		stored.Principal != operation.Principal || stored.PolicyVersion != operation.PolicyVersion || stored.IdempotencyID != operation.IdempotencyID ||
		stored.QueueSource != operation.QueueSource || !stored.Cutoff.Equal(operation.Cutoff) || stored.SegmentID != operation.SegmentID ||
		stored.Range != operation.Range || !slices.Equal(stored.Categories, operation.Categories) {
		return storage.ErrConditionFailed
	}
	return nil
}

func (s *Service) createHistoryCleanupIdempotency(ctx context.Context, key string, idempotency historyCleanupIdempotency) error {
	err := putHistoryCleanupJSON(ctx, s.store, key, idempotency, storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	stored, _, readErr := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if stored.RequestID != idempotency.RequestID || stored.OperationID != idempotency.OperationID {
		return storage.ErrConditionFailed
	}
	return nil
}

func (s *Service) readIdempotentHistoryCleanup(ctx context.Context, key, requestHash, principal string) (historyCleanupTaskView, bool, int, error) {
	idempotency, idempotencyVersion, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupTaskView{}, false, 0, nil
	}
	if err != nil {
		return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
	}
	if !idempotency.ExpiresAt.IsZero() && !idempotency.ExpiresAt.After(time.Now().UTC()) {
		if err := s.store.DeleteIfMatch(ctx, key, idempotencyVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
		}
		return historyCleanupTaskView{}, false, 0, nil
	}
	if idempotency.RequestID != requestHash {
		return historyCleanupTaskView{}, true, http.StatusConflict, storage.ErrConditionFailed
	}
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(idempotency.OperationID))
	if errors.Is(err, storage.ErrNotFound) {
		if deleteErr := s.store.DeleteIfMatch(ctx, key, idempotencyVersion); deleteErr != nil && !errors.Is(deleteErr, storage.ErrNotFound) && !errors.Is(deleteErr, storage.ErrConditionFailed) {
			return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, deleteErr
		}
		return historyCleanupTaskView{}, false, 0, nil
	}
	if err != nil {
		return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
	}
	if operation.Principal != principal {
		return historyCleanupTaskView{}, true, http.StatusNotFound, storage.ErrNotFound
	}
	operation, err = s.refreshHistoryCleanupOperationStatus(ctx, operation)
	if err != nil {
		return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
	}
	if err := s.expireHistoryCleanupIdempotency(ctx, operation.ID); err != nil {
		return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
	}
	if !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(time.Now().UTC()) {
		if err := s.deleteHistoryCleanupIdempotencyForOperation(ctx, key, operation.ID); err != nil {
			return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
		}
		return historyCleanupTaskView{}, false, 0, nil
	}
	if operation.Status != "completed" && operation.Status != "failed" {
		if err := s.enqueueHistoryCleanupOperation(ctx, operation); err != nil {
			return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
		}
	}
	view, err := s.historyCleanupStatus(ctx, idempotency.OperationID, principal)
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupTaskView{}, true, http.StatusNotFound, err
	}
	if err != nil {
		return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, err
	}
	return view, true, http.StatusAccepted, nil
}
