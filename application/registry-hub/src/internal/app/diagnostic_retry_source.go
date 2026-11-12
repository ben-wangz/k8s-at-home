package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func (s *Service) validateRetrySource(ctx context.Context, job workqueue.Job) error {
	switch job.Kind {
	case "distribution_verification":
		var work distributionVerificationWork
		if json.Unmarshal(job.Payload, &work) != nil {
			return storage.ErrCorruptObject
		}
		_, valid, err := s.distributionVerificationSource(ctx, work)
		if err != nil {
			return err
		}
		if !valid {
			return workqueue.ErrSourceMissing
		}
		return s.validateDiagnosticGeneration(ctx, work.Authority, work.Repository, work.Digest, work.Generation)
	case "cache_verification":
		var source cacheVerificationWork
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		if err := s.validateDiagnosticGeneration(ctx, source.Identity.Authority, source.Identity.Repository, source.Identity.Digest, source.Generation); err != nil {
			return err
		}
		inspection, err := s.cache.Inspect(ctx, source.Identity.Authority, source.Identity.Repository, source.Identity.Digest, source.MediaType)
		if err != nil {
			return err
		}
		if inspection.DataVersion != source.CacheVersion || inspection.Size != source.Size || inspection.PayloadFingerprint != source.Fingerprint {
			return storage.ErrConditionFailed
		}
		if !inspection.Present && !inspection.Unknown {
			return workqueue.ErrSourceMissing
		}
	case "quota_wake":
		var source quotaWakeWork
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		current, err := s.tasks.Get(ctx, source.TaskID)
		if errors.Is(err, storage.ErrNotFound) {
			return workqueue.ErrSourceMissing
		}
		if err != nil {
			return err
		}
		if current.Generation != source.Generation || current.RoundID != source.RoundID || current.QuotaWakeRevision != source.Revision || current.ProtectionSessionID != source.SessionID {
			return storage.ErrConditionFailed
		}
	case "terminal_cleanup":
		var source terminalCleanupWork
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		current, err := s.tasks.Get(ctx, source.TaskID)
		if errors.Is(err, storage.ErrNotFound) {
			return workqueue.ErrSourceMissing
		}
		if err != nil {
			return err
		}
		if !source.matches(current) {
			return storage.ErrConditionFailed
		}
	case "inventory":
		var source inventoryManifestWork
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		if err := s.validateDiagnosticGeneration(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest, source.ContentGeneration); err != nil {
			return err
		}
		if source.Identity.Reference != "" {
			if err := s.validateDiagnosticGeneration(ctx, source.Identity.Authority, source.Identity.Repository, source.Identity.Reference, source.Generation); err != nil {
				return err
			}
		}
		inspection, err := s.cache.InspectAny(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest, source.MediaType)
		if err != nil {
			return err
		}
		if inspection.Unknown {
			return storage.ErrUnavailable
		}
		if !inspection.Present {
			content, err := s.inventory.Content(ctx, source.Identity, source.Digest)
			if errors.Is(err, storage.ErrNotFound) || err == nil && (!content.ClosureKnown || content.Generation != source.ContentGeneration) {
				return workqueue.ErrSourceMissing
			}
			return err
		}
	case "execution":
		var source inventoryExecutionFact
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		return s.validateDiagnosticGeneration(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest, source.ContentGeneration)
	case "notify":
		var source blobNotification
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		current, err := s.records.GetBlobIntent(ctx, source.IntentID)
		if errors.Is(err, storage.ErrNotFound) {
			return workqueue.ErrSourceMissing
		}
		if err != nil {
			return err
		}
		if current.Generation != source.Generation || current.DemandRevision != source.DemandRevision || current.State != source.State || current.CacheVersion != source.CacheVersion {
			return storage.ErrConditionFailed
		}
	case "wake_root":
		var root string
		if json.Unmarshal(job.Payload, &root) != nil {
			return storage.ErrCorruptObject
		}
		task, err := s.tasks.Get(ctx, root)
		if errors.Is(err, storage.ErrNotFound) {
			return workqueue.ErrSourceMissing
		}
		if err != nil {
			return err
		}
		if task.Status == tasks.Cancelled || task.Status == tasks.Failed || task.Status == tasks.Solidified {
			return storage.ErrConditionFailed
		}
	case "download_cleanup":
		var source downloadCleanupWork
		if json.Unmarshal(job.Payload, &source) != nil {
			return storage.ErrCorruptObject
		}
		current, err := s.downloadManager().CleanupTicketCurrent(ctx, source.CleanupTicket)
		if err != nil {
			return err
		}
		if !current {
			present, err := s.downloadManager().HasState(ctx, source.Identity, source.Digest)
			if err != nil {
				return err
			}
			if present {
				return storage.ErrConditionFailed
			}
		}
	default:
		return storage.ErrConditionFailed
	}
	return nil
}

func (s *Service) validateDiagnosticGeneration(ctx context.Context, authority, repository, reference string, expected uint64) error {
	current, err := s.generation(ctx, authority, repository, reference)
	if err != nil {
		return err
	}
	if current.Tombstone || current.Generation != expected {
		return storage.ErrConditionFailed
	}
	return nil
}
