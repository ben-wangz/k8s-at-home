package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
)

const diagnosticPendingBatchPrefix = "diagnostics/v2/event-batch-pending/"

type diagnosticPendingSource struct {
	SourceKey       string `json:"sourceKey"`
	SourceVersion   string `json:"sourceVersion,omitempty"`
	PreviousEventID string `json:"previousEventID,omitempty"`
	FirstEventID    string `json:"firstEventID"`
	LastEventID     string `json:"lastEventID"`
	EventCount      int    `json:"eventCount"`
	ContentSHA256   string `json:"contentSHA256"`
}

type diagnosticPendingBatch struct {
	Date             string                    `json:"date"`
	Critical         bool                      `json:"critical"`
	FirstAdmittedAt  time.Time                 `json:"firstAdmittedAt"`
	OrdinaryDeadline time.Time                 `json:"ordinaryDeadline,omitempty"`
	CriticalDeadline time.Time                 `json:"criticalDeadline,omitempty"`
	Events           []diagnostics.Event       `json:"events"`
	Sources          []diagnosticPendingSource `json:"sources"`
}

type diagnosticPendingPartition struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Date          string                  `json:"date"`
	Batch         *diagnosticPendingBatch `json:"batch,omitempty"`
}

func diagnosticPendingBatchKey(date string) string {
	return diagnosticPendingBatchPrefix + date + ".json"
}

func (batch diagnosticPendingBatch) deadline() time.Time {
	if batch.Critical {
		return batch.CriticalDeadline
	}
	return batch.OrdinaryDeadline
}

func diagnosticPendingBatchSize(events []diagnostics.Event) (int, error) {
	size := 256
	for _, event := range events {
		encoded, err := json.Marshal(event)
		if err != nil {
			return 0, err
		}
		size += len(encoded) + 1
	}
	return size, nil
}

func diagnosticPendingBatchFits(events []diagnostics.Event, date string) bool {
	if len(events) == 0 || len(events) > diagnostics.BatchMaxEvents {
		return false
	}
	critical := diagnosticCriticalEvent(events[0])
	for _, event := range events {
		if event.ID == "" || event.OccurredAt.IsZero() || diagnostics.EventBatchDate(event) != date || diagnosticCriticalEvent(event) != critical {
			return false
		}
	}
	size, err := diagnosticPendingBatchSize(events)
	return err == nil && size <= diagnostics.BatchMaxBytes
}

func diagnosticEventRangeHash(events []diagnostics.Event) (string, error) {
	encoded, err := json.Marshal(events)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:]), nil
}

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

func diagnosticPartitionLeaseKey(date string) string {
	return "diagnostics/event-batch-writer/" + date
}

func diagnosticSourceLeaseKey(source string) string {
	return "diagnostics/event-outbox/" + diagnostics.ID("source-lease", source)
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
			if readErr != nil {
				return diagnosticPendingSource{}, false, readErr
			}
			if partition.SchemaVersion != 1 || partition.Date == "" || partition.Batch == nil {
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
	return s.withDiagnosticFencedLease(ctx, diagnosticPartitionLeaseKey(date), func(guarded context.Context) error {
		key := diagnosticPendingBatchKey(date)
		partition, version, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			partition, version, err = diagnosticPendingPartition{}, "", nil
		}
		if err != nil {
			return err
		}
		if partition.SchemaVersion != 0 && (partition.SchemaVersion != 1 || partition.Date != date) {
			return storage.ErrCorruptObject
		}
		if batch := partition.Batch; batch != nil {
			if batch.Critical != diagnosticCriticalEvent(events[0]) ||
				!diagnosticPendingBatchFits(append(append([]diagnostics.Event(nil), batch.Events...), events...), date) ||
				!batch.deadline().After(time.Now().UTC()) {
				if err := s.flushDiagnosticPendingPartitionLocked(guarded, date, partition, version); err != nil {
					return err
				}
				partition, version = diagnosticPendingPartition{}, ""
			}
		}
		batch := partition.Batch
		if batch == nil {
			batch = &diagnosticPendingBatch{
				Date: date, Critical: diagnosticCriticalEvent(events[0]), FirstAdmittedAt: admittedAt,
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
		partition = diagnosticPendingPartition{SchemaVersion: 1, Date: date, Batch: batch}
		if err := s.saveDiagnosticPendingPartition(guarded, date, partition); err != nil {
			return err
		}
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

func (s *Service) flushDueDiagnosticBatches(ctx context.Context, now time.Time) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	token := ""
	var result error
	for {
		entries, next, err := storage.Scan(ctx, s.store, diagnosticPendingBatchPrefix, token, 100)
		if err != nil {
			return errors.Join(result, err)
		}
		for _, entry := range entries {
			partition, _, readErr := inventory.Read[diagnosticPendingPartition](ctx, s.store, entry.Key)
			if readErr != nil {
				result = errors.Join(result, readErr)
				continue
			}
			date, validKey := diagnosticPendingKeyDate(entry.Key)
			if !validKey || partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil {
				result = errors.Join(result, storage.ErrCorruptObject)
				continue
			}
			if !partition.Batch.deadline().After(now) {
				if err := s.flushDiagnosticPendingPartition(ctx, date, false); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
					result = errors.Join(result, err)
				}
			}
		}
		if next == "" {
			return result
		}
		token = next
	}
}

func (s *Service) flushDiagnosticPendingPartition(ctx context.Context, date string, force bool) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	return s.withDiagnosticFencedLease(ctx, diagnosticPartitionLeaseKey(date), func(guarded context.Context) error {
		key := diagnosticPendingBatchKey(date)
		partition, version, err := inventory.Read[diagnosticPendingPartition](guarded, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil {
			return storage.ErrCorruptObject
		}
		if !force && partition.Batch.deadline().After(time.Now().UTC()) {
			return nil
		}
		return s.flushDiagnosticPendingPartitionLocked(guarded, date, partition, version)
	})
}

func (s *Service) flushDiagnosticPendingPartitionLocked(ctx context.Context, date string, partition diagnosticPendingPartition, version string) error {
	if partition.SchemaVersion != 1 || partition.Date != date || partition.Batch == nil ||
		!diagnosticPendingBatchFits(partition.Batch.Events, date) || len(partition.Batch.Sources) == 0 {
		return storage.ErrCorruptObject
	}
	pending := *partition.Batch
	if s.metrics != nil {
		s.metrics.recordDiagnosticWakeLatency("admission_to_flush_start", time.Since(pending.FirstAdmittedAt))
	}
	batch, err := (diagnostics.BatchLog{
		Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: s.diagnosticsConfig().EventRetention,
	}).Append(ctx, pending.Events, pending.Critical)
	gapReason := ""
	if errors.Is(err, diagnostics.ErrHistoryCapacity) {
		gapReason = "batch_budget_capacity"
	} else if errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
		gapReason = "event_history_expired"
	} else if err != nil {
		return err
	}
	for _, source := range pending.Sources {
		disposition := diagnosticEventDisposition{
			SourceKey: source.SourceKey, SourceVersion: source.SourceVersion,
			FirstEventID: source.FirstEventID, LastEventID: source.LastEventID, EventCount: source.EventCount,
			Disposition: "archived", BatchID: batch.ID, BatchSHA256: batch.ContentSHA256,
		}
		if gapReason != "" {
			gapID, gapErr := s.recordDiagnosticOutboxGapRange(ctx, source.SourceKey, source.SourceVersion,
				source.FirstEventID, source.LastEventID, source.EventCount, gapReason)
			if gapErr != nil {
				return gapErr
			}
			disposition.Disposition, disposition.Reason, disposition.GapID = "discarded_by_policy", gapReason, gapID
		}
		if err := s.saveDiagnosticPendingDisposition(ctx, source, disposition); err != nil {
			return err
		}
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	deleteErr := s.store.DeleteIfMatch(ctx, diagnosticPendingBatchKey(date), version)
	if errors.Is(deleteErr, storage.ErrUnknownCommit) {
		_, headErr := s.store.Head(ctx, diagnosticPendingBatchKey(date))
		if errors.Is(headErr, storage.ErrNotFound) {
			deleteErr = nil
		} else if headErr != nil {
			deleteErr = errors.Join(deleteErr, headErr)
		}
	}
	if errors.Is(deleteErr, storage.ErrNotFound) {
		deleteErr = nil
	}
	if deleteErr != nil {
		return deleteErr
	}
	for _, source := range pending.Sources {
		s.queueDiagnosticSourceWake(source.SourceKey, time.Now().UTC())
	}
	return nil
}

func (s *Service) saveDiagnosticPendingDisposition(ctx context.Context, source diagnosticPendingSource, disposition diagnosticEventDisposition) error {
	key := diagnostics.Prefix + "collected/" + diagnostics.ID(source.SourceKey) + ".json"
	return inventory.Update(ctx, s.store, key, func(current *eventCollectionReceipt) error {
		if source.SourceVersion != "" {
			if current.SourceVersion != "" && current.SourceVersion != source.SourceVersion {
				return storage.ErrConditionFailed
			}
			current.SourceVersion = source.SourceVersion
		}
		for _, previous := range current.Dispositions {
			if previous.SourceVersion == disposition.SourceVersion && previous.FirstEventID == disposition.FirstEventID && previous.LastEventID == disposition.LastEventID {
				if previous.Disposition != disposition.Disposition || previous.BatchID != disposition.BatchID || previous.BatchSHA256 != disposition.BatchSHA256 || previous.GapID != disposition.GapID {
					return storage.ErrCorruptObject
				}
				return nil
			}
		}
		if current.LastEventID != source.PreviousEventID {
			return storage.ErrConditionFailed
		}
		current.Dispositions = appendDisposition(current.Dispositions, disposition)
		current.LastEventID = source.LastEventID
		if disposition.Disposition == "discarded_by_policy" {
			current.Truncated = true
			current.GapIDs = appendUniqueString(current.GapIDs, disposition.GapID)
		}
		current.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func diagnosticPendingKeyDate(key string) (string, bool) {
	name := strings.TrimPrefix(key, diagnosticPendingBatchPrefix)
	if !strings.HasSuffix(name, ".json") {
		return "", false
	}
	date := strings.TrimSuffix(name, ".json")
	if len(date) != 8 {
		return "", false
	}
	if _, err := time.Parse("20060102", date); err != nil {
		return "", false
	}
	return date, true
}

func (s *Service) saveDiagnosticPendingPartition(ctx context.Context, date string, partition diagnosticPendingPartition) error {
	if partition.Date != date || partition.SchemaVersion != 1 || partition.Batch == nil {
		return fmt.Errorf("invalid diagnostic pending partition: %w", storage.ErrCorruptObject)
	}
	return inventory.Update(ctx, s.store, diagnosticPendingBatchKey(date), func(current *diagnosticPendingPartition) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || current.Date != date) {
			return storage.ErrCorruptObject
		}
		*current = partition
		return nil
	})
}
