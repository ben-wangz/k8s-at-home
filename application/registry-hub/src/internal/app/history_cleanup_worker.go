package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
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
	if err != nil {
		return progress, false, err
	}
	categories, categoryErr := normalizeHistoryCleanupCategories(operation.Categories)
	if categoryErr != nil || operation.SchemaVersion != 1 || operation.ID != job.ID || operation.PreviewID != payload.PreviewID ||
		operation.Principal != payload.Principal || operation.PolicyVersion != payload.PolicyVersion ||
		payload.PolicyVersion != historyCleanupPolicyVersion || !slices.Equal(categories, operation.Categories) ||
		operation.Cutoff.IsZero() || operation.Cutoff.After(time.Now().UTC()) {
		return progress, false, storage.ErrCorruptObject
	}
	if progress.Counts == nil {
		progress.Counts = map[string]historyCleanupCounts{}
		for _, category := range operation.Categories {
			progress.Counts[category] = historyCleanupCounts{SkipReasons: map[string]int64{}, Coverage: historyCleanupCoverage(category)}
		}
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
	if category == "diagnostic_events" && progress.PrefixIndex == 0 && !progress.EventWatermarkAdvanced {
		if err := diagnostics.AdvanceEventHistoryWatermark(ctx, s.store, operation.Cutoff.UTC().Format("20060102")); err != nil {
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

func (s *Service) saveHistoryCleanupOperation(ctx context.Context, identity historyCleanupOperation, progress historyCleanupProgress, complete bool) error {
	key := historyCleanupOperationKey(identity.ID)
	if err := inventory.Update(ctx, s.store, key, func(operation *historyCleanupOperation) error {
		if operation.SchemaVersion != 1 || operation.ID != identity.ID || operation.PreviewID != identity.PreviewID || operation.Principal != identity.Principal {
			return storage.ErrCorruptObject
		}
		operation.Counts = progress.Counts
		if operation.Status != "completed" {
			operation.Status = "running"
		}
		if complete {
			if operation.CompletedAt.IsZero() {
				operation.Status, operation.CompletedAt = "completed", time.Now().UTC()
			}
			operation.Status = "completed"
			if operation.ExpiresAt.IsZero() {
				operation.ExpiresAt = operation.CompletedAt.Add(s.diagnosticsConfig().HistoryCleanupResultRetention)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if complete {
		return s.expireHistoryCleanupIdempotency(ctx, identity.ID)
	}
	return nil
}

func (s *Service) expireHistoryCleanupIdempotency(ctx context.Context, operationID string) error {
	operation, _, err := inventory.Read[historyCleanupOperation](ctx, s.store, historyCleanupOperationKey(operationID))
	if err != nil {
		return err
	}
	if operation.IdempotencyID == "" || operation.ExpiresAt.IsZero() {
		return nil
	}
	key := historyCleanupIdempotencyPrefix + operation.IdempotencyID + ".json"
	idempotency, _, err := inventory.Read[historyCleanupIdempotency](ctx, s.store, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if idempotency.OperationID != operationID {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, s.store, key, func(value *historyCleanupIdempotency) error {
		if value.OperationID != operationID {
			return storage.ErrCorruptObject
		}
		value.ExpiresAt = operation.ExpiresAt
		return nil
	})
}
