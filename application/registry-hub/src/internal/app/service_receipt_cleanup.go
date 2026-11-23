package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

func resetReceiptCleanupPreviewCounts(progress *sourceSweepCheckpoint) {
	if progress.PreviewCountRound == progress.Round {
		return
	}
	progress.Status.Counts = map[string]int64{}
	progress.Status.Operations = 0
	progress.PreviewCountRound = progress.Round
}

func (s *Service) cleanupWorkReceipt(ctx context.Context, entry storage.Entry, progress *sourceSweepCheckpoint, preview bool) error {
	kind, id, ok := receiptWorkIdentity(entry.Key)
	if !ok {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	receipt, version, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			incrementSweepCount(progress, "missing", 1)
			return nil
		}
		return err
	}
	if receipt.ID != id || receipt.Kind != kind || receipt.State != "done" {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	if version == "" {
		version = sourceEntryVersion(entry)
	}
	terminalAt := receipt.CompletedAt
	if terminalAt.IsZero() {
		if entry.ModTime == 0 {
			incrementSweepCount(progress, "skipped", 1)
			return nil
		}
		terminalAt = time.Unix(0, entry.ModTime).UTC()
		incrementSweepCount(progress, "terminal_time_modtime_fallback", 1)
	}
	if time.Since(terminalAt) < s.diagnosticsConfig().FullWorkReceiptRetention {
		incrementSweepCount(progress, "skipped", 1)
		return nil
	}
	projectionKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(entry.Key) + ".json"
	projection, _, err := inventory.Read[diagnosticReceiptArchive](ctx, s.store, projectionKey)
	if errors.Is(err, storage.ErrNotFound) || err == nil && (!projection.Complete || !projection.HistoryProjected || !projection.OutboxComplete ||
		projection.OutboxDispositionVersion != 1 || projection.ProjectionVersion != diagnosticReceiptProjectionVersion ||
		projection.SourceKey != entry.Key || projection.SourceVersion != version) {
		incrementSweepCount(progress, "protected_outbox_pending", 1)
		return nil
	}
	if err != nil {
		return err
	}
	if kind == "execution" && !projection.ExecutionFactProjected {
		incrementSweepCount(progress, "protected_execution_fact_pending", 1)
		return nil
	}
	if kind == "execution" && (!projection.ExecutionVerificationDone || !projection.ExecutionSourcesVerified) {
		incrementSweepCount(progress, "protected_execution_sources_unverified", 1)
		return nil
	}
	if kind == "execution" {
		compact, _, compactErr := inventory.Read[compactExecutionFact](ctx, s.store, compactExecutionFactKey(id))
		var fact inventoryExecutionFact
		if compactErr != nil {
			if errors.Is(compactErr, storage.ErrNotFound) {
				incrementSweepCount(progress, "protected_execution_fact_pending", 1)
				return nil
			}
			return compactErr
		}
		if json.Unmarshal(receipt.Payload, &fact) != nil || compact.SchemaVersion != 1 || compact.WorkID != id ||
			compact.SourceRevision != workqueue.SourceRevision(receipt) || compact.State != "done" || compact.CompletedAt.IsZero() ||
			!sameExecutionFact(compact.Fact, fact) {
			return storage.ErrCorruptObject
		}
	}
	if s.protectVerificationReceiptReference(ctx, kind, id, receipt, progress) {
		return nil
	}
	if err := s.ensureNoActiveReceiptWork(ctx, kind, id); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) {
			incrementSweepCount(progress, "protected_active_work", 1)
			return nil
		}
		return err
	}
	if preview {
		current, err := s.store.Head(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			incrementSweepCount(progress, "missing", 1)
			return nil
		}
		if err != nil {
			return err
		}
		if sourceEntryVersion(current) != version || entry.Version != "" && current.Version != entry.Version {
			incrementSweepCount(progress, "protected_version_changed", 1)
			return nil
		}
		incrementSweepCount(progress, "eligible", 1)
		incrementSweepCount(progress, "eligible_bytes", current.Size)
		return nil
	}
	queue := workqueue.Queue{Store: s.store}
	if err := queue.PersistCompletionMarker(ctx, receipt); err != nil {
		return err
	}
	if err := s.ensureNoActiveReceiptWork(ctx, kind, id); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) {
			incrementSweepCount(progress, "protected_active_work", 1)
			return nil
		}
		return err
	}
	if s.protectVerificationReceiptReference(ctx, kind, id, receipt, progress) {
		return nil
	}
	current, err := s.store.Head(ctx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		incrementSweepCount(progress, "missing", 1)
		return nil
	}
	if err != nil {
		return err
	}
	if sourceEntryVersion(current) != version || entry.Version != "" && current.Version != entry.Version {
		incrementSweepCount(progress, "protected_version_changed", 1)
		return nil
	}
	err = s.store.DeleteIfMatch(ctx, entry.Key, current.Version)
	if errors.Is(err, storage.ErrNotFound) {
		err = nil
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		_, headErr := s.store.Head(ctx, entry.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			err = nil
		} else if headErr != nil {
			return errors.Join(err, headErr)
		}
	}
	if err != nil {
		return err
	}
	incrementSweepCount(progress, "deleted", 1)
	return nil
}

func (s *Service) protectVerificationReceiptReference(ctx context.Context, kind, id string, receipt workqueue.Job, progress *sourceSweepCheckpoint) bool {
	if kind != "distribution_verification" {
		return false
	}
	var work distributionVerificationWork
	if json.Unmarshal(receipt.Payload, &work) != nil || work.TaskID == "" || work.Authority == "" ||
		work.Repository == "" || work.RoundID == "" || len(work.TaskID) != 32 {
		incrementSweepCount(progress, "protected_verification_identity_unknown", 1)
		return true
	}
	if _, err := hex.DecodeString(work.TaskID); err != nil {
		incrementSweepCount(progress, "protected_verification_identity_unknown", 1)
		return true
	}
	if s.tasks == nil {
		incrementSweepCount(progress, "protected_verification_task_unavailable", 1)
		return true
	}
	task, err := s.tasks.Get(ctx, work.TaskID)
	if errors.Is(err, storage.ErrNotFound) {
		return false
	}
	if err != nil {
		incrementSweepCount(progress, "protected_verification_task_unavailable", 1)
		return true
	}
	if task.Generation == work.TaskGeneration && task.RoundID == work.RoundID && task.Authority == work.Authority &&
		task.Repository == work.Repository && task.VerificationWorkID == id {
		incrementSweepCount(progress, "protected_verification_result_unconsumed", 1)
		return true
	}
	return false
}

func (s *Service) ensureNoActiveReceiptWork(ctx context.Context, kind, id string) error {
	active, _, err := inventory.Read[workqueue.Job](ctx, s.store, workqueue.Key(kind, id))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if active.ID != id || active.Kind != kind {
		return storage.ErrCorruptObject
	}
	return storage.ErrConditionFailed
}

func receiptWorkIdentity(key string) (string, string, bool) {
	relative := strings.TrimPrefix(key, "background/v1/receipts/")
	parts := strings.SplitN(relative, "/", 2)
	if len(parts) != 2 || parts[0] == "" || !strings.HasSuffix(parts[1], ".json") {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".json"), true
}
