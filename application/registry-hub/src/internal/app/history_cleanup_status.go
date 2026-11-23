package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
