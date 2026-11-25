package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"sort"
	"time"
)

func (s *Service) enqueueHistoryCleanupOperation(ctx context.Context, operation historyCleanupOperation) error {
	if operation.QueueSource == "" || workqueue.ID(historyCleanupWorkKind, operation.QueueSource) != operation.ID ||
		operation.PreviewID == "" || operation.Principal == "" || operation.PolicyVersion != historyCleanupPolicyVersion {
		return storage.ErrCorruptObject
	}
	categories, err := normalizeHistoryCleanupCategories(operation.Categories)
	if err != nil || !slices.Equal(categories, operation.Categories) || operation.Cutoff.IsZero() {
		return storage.ErrCorruptObject
	}
	payload := historyCleanupJobPayload{PreviewID: operation.PreviewID, Principal: operation.Principal, PolicyVersion: operation.PolicyVersion, SegmentID: operation.SegmentID}
	job, err := s.backgroundQueue().Enqueue(ctx, historyCleanupWorkKind, operation.QueueSource, payload, operation.CreatedAt)
	if err != nil {
		return err
	}
	if job.ID != operation.ID {
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Service) readHistoryCleanupOperations(ctx context.Context) ([]historyCleanupOperation, error) {
	operations := make([]historyCleanupOperation, 0, historyCleanupMaxResults+1)
	now := time.Now().UTC()
	err := s.scanHistoryCleanupRecords(ctx, historyCleanupOperationPrefix, func(entry storage.Entry) error {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !operation.ExpiresAt.IsZero() && !operation.ExpiresAt.After(now) {
			return nil
		}
		operations = append(operations, operation)
		sort.Slice(operations, func(i, j int) bool { return operations[i].CreatedAt.After(operations[j].CreatedAt) })
		if len(operations) > historyCleanupMaxResults {
			operations = operations[:historyCleanupMaxResults]
		}
		return nil
	})
	return operations, err
}

func (s *Service) ensureHistoryCleanupCapacity(ctx context.Context) error {
	active := 0
	return s.scanHistoryCleanupRecords(ctx, historyCleanupOperationPrefix, func(entry storage.Entry) error {
		operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		operation, err = s.refreshHistoryCleanupOperationStatus(ctx, operation)
		if err != nil {
			return err
		}
		if operation.Status != "completed" && operation.Status != "failed" {
			active++
		}
		if active >= historyCleanupMaxResults {
			return errHistoryCleanupCapacity
		}
		return nil
	})
}

func (s *Service) refreshHistoryCleanupOperationStatus(ctx context.Context, operation historyCleanupOperation) (historyCleanupOperation, error) {
	if operation.Status == "completed" || operation.Status == "failed" {
		return operation, nil
	}
	job, err := s.backgroundQueue().Read(ctx, historyCleanupWorkKind, operation.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return operation, nil
	}
	if err != nil {
		return historyCleanupOperation{}, err
	}
	return s.refreshHistoryCleanupOperationFromJob(ctx, operation, job)
}

func (s *Service) refreshHistoryCleanupOperationFromJob(ctx context.Context, operation historyCleanupOperation, job workqueue.Job) (historyCleanupOperation, error) {
	if job.State != "failed" && job.State != "done" {
		return operation, nil
	}
	terminalAt := job.CompletedAt
	status := "completed"
	if job.State == "failed" {
		status = "failed"
		terminalAt = job.ErrorAt
	}
	if terminalAt.IsZero() {
		terminalAt = job.AttemptFinishedAt
	}
	if terminalAt.IsZero() {
		terminalAt = job.CreatedAt
	}
	err := inventory.Update(ctx, s.store, historyCleanupOperationKey(operation.ID), func(current *historyCleanupOperation) error {
		if current.SchemaVersion != 1 || current.ID != operation.ID {
			return storage.ErrCorruptObject
		}
		if current.Status == "completed" || current.Status == "failed" {
			return nil
		}
		current.Status = status
		current.CompletedAt = terminalAt
		if current.ExpiresAt.IsZero() {
			current.ExpiresAt = terminalAt.Add(s.diagnosticsConfig().HistoryCleanupResultRetention)
		}
		return nil
	})
	if err != nil {
		return historyCleanupOperation{}, err
	}
	updated, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(operation.ID))
	if err != nil {
		return historyCleanupOperation{}, err
	}
	if err := s.expireHistoryCleanupIdempotency(ctx, operation.ID); err != nil {
		return historyCleanupOperation{}, err
	}
	return updated, nil
}
