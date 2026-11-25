package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

func (s *Service) withDiagnosticFencedLease(ctx context.Context, key string, work func(context.Context) error) error {
	manager := lease.NewManager(s.store)
	leaseCtx, holder, release, err := s.acquireBackgroundUnitLease(ctx, "diagnostic_source_events", manager, key,
		s.instanceID+"/"+requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return storage.ErrConditionFailed
	}
	if err != nil {
		return err
	}
	guarded := inventory.WithWriteGuard(leaseCtx, func(check context.Context) error { return manager.Validate(check, holder) })
	workErr := work(guarded)
	return errors.Join(workErr, release())
}

func (s *Service) withDiagnosticSourceFencedLease(ctx context.Context, source string, work func(context.Context) error) error {
	manager := lease.NewManager(s.store)
	owner := s.instanceID + "/" + requestID()
	legacyCtx, legacyHolder, releaseLegacy, err := s.acquireBackgroundUnitLease(ctx, "diagnostic_source_events", manager,
		diagnosticSourceLeaseKey(source), owner, time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return storage.ErrConditionFailed
	}
	if err != nil {
		return err
	}
	shardCtx, shardHolder, releaseShard, err := s.acquireBackgroundUnitLease(legacyCtx, "diagnostic_source_events", manager,
		diagnosticSourceLeaseShardKey(source), owner, time.Minute)
	if err != nil {
		if errors.Is(err, lease.ErrHeld) {
			err = storage.ErrConditionFailed
		}
		return errors.Join(err, releaseLegacy(), s.deleteReleasedDiagnosticSourceLease(ctx, legacyHolder.Key))
	}
	guarded := inventory.WithWriteGuard(shardCtx, func(check context.Context) error {
		if err := manager.Validate(check, legacyHolder); err != nil {
			return err
		}
		return manager.Validate(check, shardHolder)
	})
	workErr := work(guarded)
	releaseErr := errors.Join(releaseShard(), releaseLegacy())
	if releaseErr == nil {
		releaseErr = s.deleteReleasedDiagnosticSourceLease(ctx, legacyHolder.Key)
	}
	return errors.Join(workErr, releaseErr)
}

func (s *Service) deleteReleasedDiagnosticSourceLease(ctx context.Context, key string) error {
	_, err := lease.NewManager(s.store).DeleteInactive(ctx, key)
	return err
}

func diagnosticPartitionLeaseKey(date string) string {
	return "diagnostics/event-batch-writer/" + date
}

func diagnosticSourceLeaseKey(source string) string {
	return "diagnostics/event-outbox/" + diagnostics.ID("source-lease", source)
}

func diagnosticSourceLeaseShardKey(source string) string {
	hash := sha256.Sum256([]byte(source))
	return fmt.Sprintf("diagnostics/event-outbox-shards/%02x", hash[0])
}

func (s *Service) appendDiagnosticBatchFenced(ctx context.Context, events []diagnostics.Event, critical bool) (diagnostics.EventBatch, error) {
	if len(events) == 0 {
		return diagnostics.EventBatch{}, storage.ErrCorruptObject
	}
	date := diagnostics.EventBatchDate(events[0])
	var batch diagnostics.EventBatch
	err := s.withDiagnosticFencedLease(ctx, diagnosticPartitionLeaseKey(date), func(guarded context.Context) error {
		var appendErr error
		batch, appendErr = (diagnostics.BatchLog{
			Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: s.diagnosticsConfig().EventRetention,
		}).Append(guarded, events, critical)
		return appendErr
	})
	return batch, err
}

func (s *Service) pendingDiagnosticSource(ctx context.Context, source string) (diagnosticPendingSource, bool, error) {
	token := ""
	for {
		entries, next, err := storage.Scan(ctx, s.store, diagnosticPendingBatchPrefix, token, 100)
		if err != nil {
			return diagnosticPendingSource{}, false, err
		}
		for _, entry := range entries {
			partition, _, readErr := inventory.Read[diagnosticPendingPartition](ctx, s.store, entry.Key)
			if errors.Is(readErr, storage.ErrNotFound) {
				continue
			}
			if readErr != nil {
				if date, ok := diagnosticPendingKeyDate(entry.Key); ok {
					s.deferDiagnosticBatchRetry(date, readErr)
				}
				return diagnosticPendingSource{}, false, readErr
			}
			if partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Date == "" || partition.Batch == nil {
				return diagnosticPendingSource{}, false, storage.ErrCorruptObject
			}
			for _, pending := range partition.Batch.Sources {
				if pending.SourceKey == source {
					return pending, true, nil
				}
			}
		}
		if next == "" {
			return diagnosticPendingSource{}, false, nil
		}
		token = next
	}
}

func (s *Service) admitDiagnosticOutboxBatch(ctx context.Context, source, sourceVersion, previousEventID string, events []diagnostics.Event, admittedAt time.Time) error {
	if len(events) == 0 || !diagnosticPendingBatchFits(events, diagnostics.EventBatchDate(events[0])) {
		return storage.ErrCorruptObject
	}
	first, last := events[0].ID, events[len(events)-1].ID
	rangeHash, err := diagnosticEventRangeHash(events)
	if err != nil {
		return err
	}
	rangeInfo := diagnosticPendingSource{
		SourceKey: source, SourceVersion: sourceVersion, PreviousEventID: previousEventID,
		FirstEventID: first, LastEventID: last, EventCount: len(events), ContentSHA256: rangeHash,
	}
	if existing, found, err := s.pendingDiagnosticSource(ctx, source); err != nil {
		return err
	} else if found {
		if existing.SourceVersion == rangeInfo.SourceVersion && existing.FirstEventID == first && existing.LastEventID == last &&
			existing.EventCount == len(events) && existing.ContentSHA256 == rangeHash {
			return nil
		}
		return storage.ErrConditionFailed
	}
	date := diagnostics.EventBatchDate(events[0])
	return s.withDiagnosticPendingPartition(ctx, date, false, func(guarded context.Context) error {
		key := diagnosticPendingBatchKey(date)
		partition, version, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			partition, version, err = diagnosticPendingPartition{}, "", nil
		}
		if err != nil {
			return err
		}
		if partition.SchemaVersion != 0 && (partition.SchemaVersion != diagnosticPendingSchemaVersion || partition.Date != date || partition.Batch == nil) {
			return storage.ErrCorruptObject
		}
		if batch := partition.Batch; batch != nil {
			if batch.readyToFlush(time.Now().UTC()) || batch.Critical != diagnosticCriticalEvent(events[0]) ||
				!diagnosticPendingBatchFits(append(append([]diagnostics.Event(nil), batch.Events...), events...), date) {
				if err := s.flushDiagnosticPendingPartitionLocked(guarded, date, partition, version); err != nil {
					return err
				}
				partition, version = diagnosticPendingPartition{}, ""
			}
		}
		batch := partition.Batch
		if batch == nil {
			batch = &diagnosticPendingBatch{
				Date: date, Critical: diagnosticCriticalEvent(events[0]), FirstAdmittedAt: admittedAt, Appendable: true,
				OrdinaryDeadline: admittedAt.Add(s.diagnosticsConfig().OrdinaryEventFlushInterval),
				Events:           append([]diagnostics.Event(nil), events...), Sources: []diagnosticPendingSource{rangeInfo},
			}
			if batch.FirstAdmittedAt.IsZero() {
				batch.FirstAdmittedAt = time.Now().UTC()
				batch.OrdinaryDeadline = batch.FirstAdmittedAt.Add(s.diagnosticsConfig().OrdinaryEventFlushInterval)
			}
		} else {
			batch.Events = append(batch.Events, events...)
			batch.Sources = append(batch.Sources, rangeInfo)
		}
		if batch.Critical {
			if batch.CriticalDeadline.IsZero() {
				batch.CriticalDeadline = admittedAt.Add(s.diagnosticsConfig().CriticalEventFlushInterval)
				if admittedAt.IsZero() {
					batch.CriticalDeadline = time.Now().UTC().Add(s.diagnosticsConfig().CriticalEventFlushInterval)
				}
			}
		}
		partition = diagnosticPendingPartition{SchemaVersion: diagnosticPendingSchemaVersion, Date: date, Batch: batch}
		if err := s.saveDiagnosticPendingPartition(guarded, date, partition); err != nil {
			return err
		}
		s.signalDiagnosticBatchWake()
		if len(batch.Events) >= diagnostics.BatchMaxEvents {
			stored, storedVersion, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
			if err != nil {
				return err
			}
			return s.flushDiagnosticPendingPartitionLocked(guarded, date, stored, storedVersion)
		}
		return nil
	})
}
