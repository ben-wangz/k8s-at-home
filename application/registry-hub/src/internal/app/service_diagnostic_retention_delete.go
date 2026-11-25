package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const diagnosticEventWatermarkWorker = "diagnostic_retention_event_watermark"

func (s *Service) sealExpiredDiagnosticEventDay(ctx context.Context, entry storage.Entry) (bool, error) {
	date, valid := historyCleanupEventDate(entry.Key)
	if !valid {
		return false, storage.ErrCorruptObject
	}
	if until := s.diagnosticSealBackoff.dueAt(date); until.After(time.Now()) {
		return false, diagnosticPartitionDeferred{until: until}
	}
	sealed, err := diagnostics.SealExpiredEventDay(ctx, s.store, date, s.diagnosticsConfig().EventRetention, time.Now().UTC())
	if err == nil {
		s.diagnosticSealBackoff.clear(date)
	} else if !errors.Is(ctx.Err(), context.Canceled) && !errors.Is(err, errHistoryCleanupRequestBudget) {
		s.diagnosticSealBackoff.deferRetry(date)
	}
	return sealed, err
}

func (s *Service) cleanupLegacyHistoryObservation(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint, age time.Duration, classify func(context.Context, storage.Entry, time.Time) (historyCleanupCandidate, error)) error {
	now := time.Now().UTC()
	if entry.ModTime == 0 || now.Sub(time.Unix(0, entry.ModTime)) < age {
		incrementSweepCount(p, "skipped", 1)
		return nil
	}
	candidate, err := classify(ctx, entry, now)
	if err != nil {
		return err
	}
	if !candidate.Eligible {
		incrementSweepCount(p, "skipped", 1)
		incrementSweepCount(p, candidate.Reason, 1)
		return nil
	}
	deleted, missing, err := s.deleteHistoryEntryConditional(ctx, entry)
	if deleted {
		incrementSweepCount(p, "deleted", 1)
	} else if missing {
		incrementSweepCount(p, "missing", 1)
	}
	return err
}

func (s *Service) cleanupDiagnosticSourceLease(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	deleted, err := lease.NewManager(s.store).DeleteInactive(ctx, entry.Key)
	if err != nil {
		return err
	}
	if !deleted {
		incrementSweepCount(p, "skipped", 1)
		return nil
	}
	incrementSweepCount(p, "deleted", 1)
	return nil
}

func (s *Service) deleteDiagnosticObservation(ctx context.Context, entry storage.Entry) error {
	if entry.Version == "" {
		current, err := s.store.Head(ctx, entry.Key)
		if err != nil {
			return err
		}
		if entry.ModTime != current.ModTime {
			return storage.ErrConditionFailed
		}
		entry = current
	}
	err := s.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, headErr := s.store.Head(ctx, entry.Key)
		if errors.Is(headErr, storage.ErrNotFound) {
			return nil
		}
		if headErr != nil {
			return errors.Join(err, headErr)
		}
		if sourceEntryVersion(current) != sourceEntryVersion(entry) {
			return storage.ErrConditionFailed
		}
	}
	return err
}
