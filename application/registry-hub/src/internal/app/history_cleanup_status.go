package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

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
		if operation.SchemaVersion != 1 || operation.ID != id || operation.PreviewID != payload.PreviewID || operation.SegmentID != payload.SegmentID {
			return historyCleanupTaskView{}, storage.ErrCorruptObject
		}
		if operation.Principal != principal {
			return historyCleanupTaskView{}, storage.ErrNotFound
		}
		if job.State == "failed" {
			operation, readErr = s.refreshHistoryCleanupOperationStatus(ctx, operation)
			if readErr != nil {
				return historyCleanupTaskView{}, readErr
			}
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
		Range: operation.Range, SegmentID: operation.SegmentID,
		Counts: operation.Counts, CreatedAt: job.CreatedAt, CompletedAt: completedAt,
		ExpiresAt: operation.ExpiresAt, LastError: job.ErrorClass, NextRetry: job.NextAttemptAt,
	}
	if view.Counts == nil {
		view.Counts = progress.Counts
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
		Range: operation.Range, SegmentID: operation.SegmentID,
		CreatedAt: operation.CreatedAt, CompletedAt: operation.CompletedAt, ExpiresAt: operation.ExpiresAt,
	}
	return addHistoryCleanupTotals(view)
}

func addHistoryCleanupTotals(view historyCleanupTaskView) historyCleanupTaskView {
	for _, counts := range view.Counts {
		view.Scanned += counts.Scanned
		view.Deleted += counts.Deleted
		view.Missing += counts.Missing
		view.Unconfirmed += counts.Unconfirmed
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
			"diagnostic_events":              fmt.Sprintf("automatic cleanup after %s; manual cleanup seals and removes selected UTC time ranges", diagnosticsConfig.EventRetention),
			"terminal_work_history":          fmt.Sprintf("%s projection history; full receipts require %s age and complete replay, execution, reference, and outbox proofs", diagnosticsConfig.TerminalHistoryRetention, diagnosticsConfig.FullWorkReceiptRetention),
			"terminal_transfer_history":      fmt.Sprintf("%s terminal projection; active source transfer state is outside the manual history scope", diagnosticsConfig.TerminalHistoryRetention),
			"storage_snapshot_history":       "manifest expiry; latest and active collection data remains protected",
			"full_work_receipts":             fmt.Sprintf("completed receipts older than %s with settled handoff and verified replacement evidence", diagnosticsConfig.FullWorkReceiptRetention),
			"diagnostic_collection_receipts": fmt.Sprintf("receipts older than %s after all source handoffs settle", diagnosticsConfig.CollectedReceiptRetention),
			"transfer_observations":          fmt.Sprintf("terminal transfer state and transfer observations older than %s", diagnosticsConfig.TransferObservationRetention),
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
		Workers:         map[string]historyCleanupWorkerSummary{},
		RangeLimits:     currentHistoryCleanupRangeLimits(),
		ReceiptBackfill: historyCleanupReceiptBackfill{AutomaticCleanupEnabled: diagnosticsConfig.FullWorkReceiptCleanupEnabled},
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
	if err := s.readHistoryCleanupEligibleVolume(ctx, &view); err != nil {
		return historyCleanupRetentionView{}, err
	}
	for _, worker := range historyCleanupRetentionWorkerKinds() {
		if !retentionCleanupKind(worker) {
			continue
		}
		status, err := readSourceScanStatus(ctx, s.store, worker)
		if err != nil {
			continue
		}
		view.Workers[worker] = historyCleanupWorkerSummary{LastSuccessAt: status.LastSuccessAt,
			LastSuccessfulRound: status.LastSuccessfulRound, PendingRetries: status.PendingRetries, Deleted: status.Counts["deleted"]}
		if !status.NextScanAt.IsZero() && (view.NextRun.IsZero() || status.NextScanAt.Before(view.NextRun)) {
			view.NextRun = status.NextScanAt
		}
	}
	if status, err := readSourceScanStatus(ctx, s.store, "history_receipts"); err == nil {
		view.ReceiptBackfill.InitialRoundVerified = !status.LastSuccessAt.IsZero() && status.PendingRetries == 0
		view.ReceiptBackfill.PendingRetries = status.PendingRetries
	}
	return view, nil
}
