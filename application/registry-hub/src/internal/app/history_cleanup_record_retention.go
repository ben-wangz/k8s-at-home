package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

var historyCleanupRecordWorkers = []struct {
	kind, prefix string
}{
	{"diagnostic_retention_cleanup_cutoff_previews", historyCleanupPreviewPrefix},
	{"diagnostic_retention_cleanup_operations", historyCleanupOperationPrefix},
	{"diagnostic_retention_cleanup_idempotency", historyCleanupIdempotencyPrefix},
	{"diagnostic_retention_cleanup_failed_previews", workqueue.Prefix + "jobs/" + historyCleanupPreviewKind + "/"},
	{"diagnostic_retention_cleanup_failed_operations", workqueue.Prefix + "jobs/" + historyCleanupWorkKind + "/"},
}

func historyCleanupRetentionWorkerKinds() []string {
	kinds := slices.Concat(backgroundScanKinds, historyCleanupMaintenanceKinds)
	for _, worker := range historyCleanupRecordWorkers {
		kinds = append(kinds, worker.kind)
	}
	return kinds
}

func (s *Service) pruneHistoryCleanupRecord(ctx context.Context, kind string, entry storage.Entry) error {
	switch kind {
	case "diagnostic_retention_cleanup_cutoff_previews":
		return s.pruneHistoryCleanupCutoffEntry(ctx, entry)
	case "diagnostic_retention_cleanup_operations":
		return s.pruneHistoryCleanupOperationEntry(ctx, entry)
	case "diagnostic_retention_cleanup_idempotency":
		return s.pruneHistoryCleanupIdempotencyEntry(ctx, entry, time.Now().UTC())
	case "diagnostic_retention_cleanup_failed_previews", "diagnostic_retention_cleanup_failed_operations":
		return s.pruneFailedHistoryCleanupJob(ctx, entry)
	default:
		return storage.ErrCorruptObject
	}
}

func (s *Service) pruneHistoryCleanupCutoffEntry(ctx context.Context, entry storage.Entry) error {
	preview, version, err := inventory.Read[historyCleanupPreview](ctx, s.store, entry.Key)
	if err != nil || preview.ExpiresAt.After(time.Now().UTC()) {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	return s.store.DeleteIfMatch(ctx, entry.Key, version)
}

func (s *Service) pruneHistoryCleanupOperationEntry(ctx context.Context, entry storage.Entry) error {
	operation, version, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	if operation.SchemaVersion != 1 || operation.ID == "" || entry.Key != historyCleanupOperationKey(operation.ID) {
		return storage.ErrCorruptObject
	}
	if operation.Status != "completed" && operation.Status != "failed" {
		return s.reconcileHistoryCleanupOperation(ctx, operation)
	}
	if operation.ExpiresAt.IsZero() || operation.ExpiresAt.After(time.Now().UTC()) {
		return nil
	}
	manager := lease.NewManager(s.store)
	holder, err := manager.Acquire(ctx, "diagnostics/history-cleanup-api", requestID(), 30*time.Second)
	if err != nil {
		return err
	}
	defer manager.Release(ctx, holder)
	entry.Version = version
	return s.deleteHistoryCleanupOperation(ctx, entry, operation)
}

func (s *Service) pruneHistoryCleanupIdempotencyEntry(ctx context.Context, entry storage.Entry, now time.Time) error {
	idempotency, version, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	if idempotency.ExpiresAt.IsZero() {
		if entry.ModTime == 0 || now.Sub(time.Unix(0, entry.ModTime)) < s.diagnosticsConfig().HistoryCleanupResultRetention {
			return nil
		}
		if _, err := s.store.Head(ctx, historyCleanupOperationKey(idempotency.OperationID)); !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	} else if idempotency.ExpiresAt.After(now) {
		return nil
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	return s.store.DeleteIfMatch(ctx, entry.Key, version)
}

func (s *Service) pruneFailedHistoryCleanupJob(ctx context.Context, entry storage.Entry) error {
	job, version, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
	if err != nil || job.State != "failed" {
		return err
	}
	if job.ID == "" || entry.Key != workqueue.Key(job.Kind, job.ID) {
		return storage.ErrCorruptObject
	}
	parent := historyCleanupOperationKey(job.ID)
	if job.Kind == historyCleanupPreviewKind {
		var payload historyCleanupRangeJobPayload
		if json.Unmarshal(job.Payload, &payload) == nil && payload.PreviewID != "" && workqueue.ID(job.Kind, payload.PreviewID) == job.ID {
			parent = historyCleanupRangePreviewKey(payload.PreviewID)
		} else {
			at := job.ErrorAt
			if at.IsZero() {
				at = job.CreatedAt
			}
			if time.Since(at) < max(time.Hour, s.diagnosticsConfig().HistoryCleanupPreviewTTL) {
				return nil
			}
			parent = ""
		}
	}
	if parent != "" {
		if _, err := s.store.Head(ctx, parent); !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	return s.deleteFailedHistoryCleanupJob(ctx, entry.Key, job, version)
}

func (s *Service) deleteFailedHistoryCleanupJob(ctx context.Context, key string, job workqueue.Job, version string) error {
	if job.State != "failed" {
		return nil
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	err := s.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) retireFailedHistoryCleanupJob(ctx context.Context, kind, id string) error {
	key := workqueue.Key(kind, id)
	job, version, err := inventory.Read[workqueue.Job](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.deleteFailedHistoryCleanupJob(ctx, key, job, version)
}
