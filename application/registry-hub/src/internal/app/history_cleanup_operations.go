package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

const historyCleanupIdempotencyPrefix = diagnostics.Prefix + "cleanup-idempotency/"

func (s *Service) pruneHistoryCleanupPreviews(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupPreviewPrefix, "", 1000)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		preview, version, err := inventory.Read[historyCleanupPreview](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if preview.ExpiresAt.After(now) {
			continue
		}
		if err := s.store.DeleteIfMatch(ctx, entry.Key, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) pruneHistoryCleanupOperations(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupOperationPrefix, "", 1000)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	operations := make([]historyCleanupOperation, 0, len(entries))
	for _, entry := range entries {
		operation, version, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.SchemaVersion != 1 || operation.ID == "" {
			return storage.ErrCorruptObject
		}
		if operation.Status == "completed" && !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(now) {
			entry.Version = version
			if err := s.deleteHistoryCleanupOperation(ctx, entry, operation); err != nil {
				return err
			}
			continue
		}
		operations = append(operations, operation)
	}
	terminal := make([]historyCleanupOperation, 0, len(operations))
	for _, operation := range operations {
		if operation.Status == "completed" {
			terminal = append(terminal, operation)
		}
	}
	sort.Slice(terminal, func(i, j int) bool { return terminal[i].CompletedAt.After(terminal[j].CompletedAt) })
	if len(terminal) > historyCleanupMaxResults {
		for _, operation := range terminal[historyCleanupMaxResults:] {
			entry, err := s.store.Head(ctx, historyCleanupOperationKey(operation.ID))
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if err := s.deleteHistoryCleanupOperation(ctx, entry, operation); err != nil {
				return err
			}
		}
	}
	return s.pruneHistoryCleanupIdempotency(ctx, now)
}

func (s *Service) deleteHistoryCleanupOperation(ctx context.Context, entry storage.Entry, operation historyCleanupOperation) error {
	if err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if operation.IdempotencyID != "" {
		if idempotency, err := s.store.Head(ctx, historyCleanupIdempotencyPrefix+operation.IdempotencyID+".json"); err == nil {
			if err := s.store.DeleteIfMatch(ctx, idempotency.Key, idempotency.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return nil
}

func (s *Service) pruneHistoryCleanupIdempotency(ctx context.Context, now time.Time) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupIdempotencyPrefix, "", 1000)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		idempotency, version, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !idempotency.ExpiresAt.IsZero() && !idempotency.ExpiresAt.After(now) {
			if err := s.store.DeleteIfMatch(ctx, entry.Key, version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
	}
	return nil
}

func (s *Service) submitHistoryCleanup(ctx context.Context, principal string, idempotencyKey string, request historyCleanupExecutionRequest) (historyCleanupTaskView, int, error) {
	if request.PreviewID == "" || !validHistoryCleanupIdempotencyKey(idempotencyKey) {
		return historyCleanupTaskView{}, http.StatusBadRequest, storage.ErrCorruptObject
	}
	requestHash := diagnostics.ID("history-cleanup-request", request.PreviewID)
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
	preview, _, err := inventory.Read[historyCleanupPreview](ctx, s.store, historyCleanupPreviewKey(request.PreviewID))
	if errors.Is(err, storage.ErrNotFound) || err == nil && preview.Principal != principal {
		return historyCleanupTaskView{}, http.StatusNotFound, storage.ErrNotFound
	}
	if err != nil {
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	if !preview.ExpiresAt.After(time.Now().UTC()) {
		return historyCleanupTaskView{}, http.StatusGone, inventory.ErrExpired
	}
	if preview.PolicyVersion != historyCleanupPolicyVersion || !preview.Complete {
		return historyCleanupTaskView{}, http.StatusConflict, storage.ErrConditionFailed
	}
	if err := s.pruneHistoryCleanupOperations(ctx); err != nil {
		return historyCleanupTaskView{}, http.StatusServiceUnavailable, err
	}
	payload := historyCleanupJobPayload{PreviewID: preview.ID, Principal: principal, PolicyVersion: preview.PolicyVersion}
	source := diagnostics.ID("history-cleanup-request", principal, idempotencyKey, requestHash)
	jobID := workqueue.ID(historyCleanupWorkKind, source)
	operation := historyCleanupOperation{
		SchemaVersion: 1, ID: jobID, PreviewID: preview.ID, Principal: principal, Categories: preview.Categories,
		Cutoff: preview.Cutoff, PolicyVersion: preview.PolicyVersion, Status: "pending", Counts: map[string]historyCleanupCounts{},
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
		return historyCleanupTaskView{}, http.StatusConflict, err
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
			stored.QueueSource != operation.QueueSource || !stored.Cutoff.Equal(operation.Cutoff) || !slices.Equal(stored.Categories, operation.Categories) {
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
		stored.QueueSource != operation.QueueSource || !stored.Cutoff.Equal(operation.Cutoff) || !slices.Equal(stored.Categories, operation.Categories) {
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
		if deleteErr := s.store.DeleteIfMatch(ctx, key, idempotencyVersion); deleteErr != nil && !errors.Is(deleteErr, storage.ErrNotFound) {
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
	if !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(time.Now().UTC()) {
		if deleteErr := s.store.DeleteIfMatch(ctx, key, idempotencyVersion); deleteErr != nil && !errors.Is(deleteErr, storage.ErrNotFound) {
			return historyCleanupTaskView{}, true, http.StatusServiceUnavailable, deleteErr
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

func (s *Service) enqueueHistoryCleanupOperation(ctx context.Context, operation historyCleanupOperation) error {
	if operation.QueueSource == "" {
		return storage.ErrCorruptObject
	}
	categories, err := normalizeHistoryCleanupCategories(operation.Categories)
	if err != nil || !slices.Equal(categories, operation.Categories) || operation.Cutoff.IsZero() {
		return storage.ErrCorruptObject
	}
	payload := historyCleanupJobPayload{PreviewID: operation.PreviewID, Principal: operation.Principal, PolicyVersion: operation.PolicyVersion}
	job, err := s.backgroundQueue().Enqueue(ctx, historyCleanupWorkKind, operation.QueueSource, payload, operation.CreatedAt)
	if err != nil {
		return err
	}
	if job.ID != operation.ID {
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Service) ensureHistoryCleanupCapacity(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupOperationPrefix, "", 1000)
	if err != nil {
		return err
	}
	active := 0
	for _, entry := range entries {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if operation.Status != "completed" {
			active++
		}
	}
	if active >= historyCleanupMaxResults {
		return errHistoryCleanupCapacity
	}
	return nil
}

func (s *Service) historyCleanupStatus(ctx context.Context, id, principal string) (historyCleanupTaskView, error) {
	job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, id)
	if err == nil {
		var payload historyCleanupJobPayload
		if json.Unmarshal(job.Payload, &payload) != nil || payload.Principal != principal {
			return historyCleanupTaskView{}, storage.ErrNotFound
		}
		operation, _, readErr := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(id))
		if errors.Is(readErr, storage.ErrNotFound) {
			return historyCleanupTaskView{}, inventory.ErrExpired
		}
		if readErr != nil {
			return historyCleanupTaskView{}, readErr
		}
		if operation.SchemaVersion != 1 || operation.ID != id || operation.PreviewID != payload.PreviewID {
			return historyCleanupTaskView{}, storage.ErrCorruptObject
		}
		if operation.Principal != principal {
			return historyCleanupTaskView{}, storage.ErrNotFound
		}
		if !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(time.Now().UTC()) {
			return historyCleanupTaskView{}, inventory.ErrExpired
		}
		return historyCleanupViewFromJob(job, operation), nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return historyCleanupTaskView{}, err
	}
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(id))
	if errors.Is(err, storage.ErrNotFound) || err == nil && operation.Principal != principal {
		return historyCleanupTaskView{}, storage.ErrNotFound
	}
	if err != nil {
		return historyCleanupTaskView{}, err
	}
	if operation.SchemaVersion != 1 || operation.ID != id {
		return historyCleanupTaskView{}, storage.ErrCorruptObject
	}
	if !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(time.Now().UTC()) {
		return historyCleanupTaskView{}, inventory.ErrExpired
	}
	return historyCleanupViewFromOperation(operation), nil
}

func historyCleanupViewFromJob(job workqueue.Job, operation historyCleanupOperation) historyCleanupTaskView {
	var progress historyCleanupProgress
	if len(job.Progress) > 0 {
		_ = json.Unmarshal(job.Progress, &progress)
	}
	completedAt := operation.CompletedAt
	if completedAt.IsZero() {
		completedAt = job.CompletedAt
	}
	view := historyCleanupTaskView{
		ID: job.ID, PreviewID: operation.PreviewID, Status: job.State, StatusURL: "/api/v1/history/cleanup/" + job.ID,
		PolicyVersion: operation.PolicyVersion, Categories: operation.Categories, Cutoff: operation.Cutoff,
		Counts: progress.Counts, CreatedAt: job.CreatedAt, CompletedAt: completedAt,
		ExpiresAt: operation.ExpiresAt, LastError: job.ErrorClass, NextRetry: job.NextAttemptAt,
	}
	if job.State == "failed" {
		view.Failed = 1
	}
	return addHistoryCleanupTotals(view)
}

func historyCleanupViewFromOperation(operation historyCleanupOperation) historyCleanupTaskView {
	view := historyCleanupTaskView{
		ID: operation.ID, PreviewID: operation.PreviewID, Status: operation.Status,
		StatusURL: "/api/v1/history/cleanup/" + operation.ID, PolicyVersion: operation.PolicyVersion,
		Categories: operation.Categories, Cutoff: operation.Cutoff, Counts: operation.Counts,
		CreatedAt: operation.CreatedAt, CompletedAt: operation.CompletedAt, ExpiresAt: operation.ExpiresAt,
	}
	return addHistoryCleanupTotals(view)
}

func addHistoryCleanupTotals(view historyCleanupTaskView) historyCleanupTaskView {
	for _, counts := range view.Counts {
		view.Scanned += counts.Scanned
		view.Deleted += counts.Deleted
		view.Missing += counts.Missing
		view.Skipped += counts.Skipped
		view.Failed += counts.Failed
		view.DeletedBytes += counts.DeletedBytes
	}
	return view
}

func validHistoryCleanupIdempotencyKey(key string) bool {
	if len(key) < 1 || len(key) > 128 || strings.TrimSpace(key) != key {
		return false
	}
	for _, value := range key {
		if value < 0x21 || value > 0x7e {
			return false
		}
	}
	return true
}

func (s *Service) historyCleanupRetention(ctx context.Context) (historyCleanupRetentionView, error) {
	diagnosticsConfig := s.diagnosticsConfig()
	view := historyCleanupRetentionView{
		PolicyVersion: historyCleanupPolicyVersion,
		Policies: map[string]string{
			"diagnostic_events":         fmt.Sprintf("automatic cleanup after %s; manual cleanup removes whole UTC partitions before the sealed cutoff date", diagnosticsConfig.EventRetention),
			"terminal_work_history":     fmt.Sprintf("%s projection history; full receipts require %s age and complete replay, execution, reference, and outbox proofs", diagnosticsConfig.TerminalHistoryRetention, diagnosticsConfig.FullWorkReceiptRetention),
			"terminal_transfer_history": fmt.Sprintf("%s terminal projection; active source transfer state is outside the manual history scope", diagnosticsConfig.TerminalHistoryRetention),
			"storage_snapshot_history":  "manifest expiry; latest and active collection data remains protected",
		},
		DurationsSeconds: map[string]int64{
			"diagnosticEventRetention":       int64(diagnosticsConfig.EventRetention.Seconds()),
			"diagnosticEventBudgetRetention": int64(diagnosticsConfig.EventBudgetRetention.Seconds()),
			"diagnosticCollectionRetention":  int64(diagnosticsConfig.CollectedReceiptRetention.Seconds()),
			"retryRequestRetention":          int64(diagnosticsConfig.RetryRequestRetention.Seconds()),
			"transferObservationRetention":   int64(diagnosticsConfig.TransferObservationRetention.Seconds()),
			"transferExpiryRetention":        int64(diagnosticsConfig.TransferExpiryRetention.Seconds()),
			"terminalHistoryRetention":       int64(diagnosticsConfig.TerminalHistoryRetention.Seconds()),
			"fullWorkReceiptRetention":       int64(diagnosticsConfig.FullWorkReceiptRetention.Seconds()),
			"cleanupPreviewTTL":              int64(diagnosticsConfig.HistoryCleanupPreviewTTL.Seconds()),
			"cleanupResultRetention":         int64(diagnosticsConfig.HistoryCleanupResultRetention.Seconds()),
			"historySweepInterval":           int64(diagnosticsConfig.HistorySweepInterval.Seconds()),
			"historySweepMaxInterval":        int64(diagnosticsConfig.HistorySweepMaxInterval.Seconds()),
			"retentionSweepInterval":         int64(diagnosticsConfig.RetentionSweepInterval.Seconds()),
			"ordinaryEventFlushInterval":     int64(diagnosticsConfig.OrdinaryEventFlushInterval.Seconds()),
			"criticalEventFlushInterval":     int64(diagnosticsConfig.CriticalEventFlushInterval.Seconds()),
		},
		KnownEligibleVolume: map[string]historyCleanupCounts{}, ObservedAt: time.Now().UTC(),
	}
	operations, err := s.readHistoryCleanupOperations(ctx)
	if err != nil {
		return historyCleanupRetentionView{}, err
	}
	if len(operations) > 0 {
		last := historyCleanupViewFromOperation(operations[0])
		if job, readErr := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, last.ID); readErr == nil {
			last = historyCleanupViewFromJob(job, operations[0])
		}
		view.LastCleanup = &last
	}
	previews, _, err := storage.Scan(ctx, s.store, historyCleanupPreviewPrefix, "", 1000)
	if err != nil {
		return historyCleanupRetentionView{}, err
	}
	var latest historyCleanupPreview
	now := time.Now().UTC()
	for _, entry := range previews {
		candidate, _, err := inventory.Read[historyCleanupPreview](ctx, s.store, entry.Key)
		if err != nil {
			continue
		}
		if candidate.Complete && candidate.ExpiresAt.After(now) && candidate.CreatedAt.After(latest.CreatedAt) {
			latest = candidate
		}
	}
	if latest.Counts != nil {
		view.KnownEligibleVolume = latest.Counts
	}
	for _, worker := range []string{"diagnostic_retention_events", "diagnostic_retention_work_history", "diagnostic_retention_transfer_history", "diagnostic_retention_storage_snapshots"} {
		status, err := readSourceScanStatus(ctx, s.store, worker)
		if err == nil && !status.NextScanAt.IsZero() && (view.NextRun.IsZero() || status.NextScanAt.Before(view.NextRun)) {
			view.NextRun = status.NextScanAt
		}
	}
	return view, nil
}

func (s *Service) readHistoryCleanupOperations(ctx context.Context) ([]historyCleanupOperation, error) {
	entries, _, err := storage.Scan(ctx, s.store, historyCleanupOperationPrefix, "", 1000)
	if err != nil {
		return nil, err
	}
	operations := make([]historyCleanupOperation, 0, len(entries))
	now := time.Now().UTC()
	for _, entry := range entries {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if operation.Status == "completed" && !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(now) {
			continue
		}
		operations = append(operations, operation)
	}
	sort.Slice(operations, func(i, j int) bool { return operations[i].CreatedAt.After(operations[j].CreatedAt) })
	if len(operations) > historyCleanupMaxResults {
		operations = operations[:historyCleanupMaxResults]
	}
	return operations, nil
}
