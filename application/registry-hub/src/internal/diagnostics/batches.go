package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

const (
	BatchPrefix           = "diagnostics/v2/event-batches/"
	BatchBudgetKey        = "diagnostics/v2/event-batch-budget.json"
	BatchMaxEvents        = 128
	BatchMaxBytes         = 64 << 10
	BatchDailyBytesLimit  = int64(16 << 20)
	BatchDailyCountLimit  = int64(4096)
	BatchGlobalBytesLimit = int64(128 << 20)
	BatchGlobalCountLimit = int64(32768)
	BatchCriticalDayBytes = int64(1 << 20)
	BatchCriticalDayCount = int64(32)
	BatchCriticalAllBytes = int64(8 << 20)
	BatchCriticalAllCount = int64(256)
)

var ErrBatchHistoryExpired = errors.New("diagnostic event batch is outside the history window")

type EventBatch struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	Date          string    `json:"date"`
	ContentSHA256 string    `json:"contentSHA256"`
	CreatedAt     time.Time `json:"createdAt"`
	Events        []Event   `json:"events"`
}

type EventBatchReservation struct {
	Date     string `json:"date"`
	Bytes    int64  `json:"bytes"`
	Critical bool   `json:"critical,omitempty"`
}

type EventBatchBudget struct {
	Reservations     map[string]EventBatchReservation `json:"reservations,omitempty"`
	RejectedBatches  uint64                           `json:"rejectedBatches,omitempty"`
	RejectedCritical uint64                           `json:"rejectedCritical,omitempty"`
}

type EventBatchBudgetSummary struct {
	Date                  string `json:"date"`
	DailyBytes            int64  `json:"dailyBytes"`
	DailyBatches          int64  `json:"dailyBatches"`
	GlobalBytes           int64  `json:"globalBytes"`
	GlobalBatches         int64  `json:"globalBatches"`
	RejectedBatches       uint64 `json:"rejectedBatches"`
	RejectedCritical      uint64 `json:"rejectedCritical"`
	DailyBytesLimit       int64  `json:"dailyBytesLimit"`
	DailyBatchesLimit     int64  `json:"dailyBatchesLimit"`
	GlobalBytesLimit      int64  `json:"globalBytesLimit"`
	GlobalBatchesLimit    int64  `json:"globalBatchesLimit"`
	DailyCriticalBytes    int64  `json:"dailyCriticalBytes"`
	DailyCriticalBatches  int64  `json:"dailyCriticalBatches"`
	GlobalCriticalBytes   int64  `json:"globalCriticalBytes"`
	GlobalCriticalBatches int64  `json:"globalCriticalBatches"`
}

type BatchPolicy struct {
	DailyBytes            int64
	DailyBatches          int64
	GlobalBytes           int64
	GlobalBatches         int64
	DailyCriticalBytes    int64
	DailyCriticalBatches  int64
	GlobalCriticalBytes   int64
	GlobalCriticalBatches int64
}

type BatchLog struct {
	Store     storage.Store
	Policy    BatchPolicy
	Retention time.Duration
}

func (log BatchLog) retention() time.Duration {
	if log.Retention > 0 {
		return log.Retention
	}
	return Retention
}

func ObservationBatchPolicy() BatchPolicy {
	return BatchPolicy{
		DailyBytes: BatchDailyBytesLimit, DailyBatches: BatchDailyCountLimit,
		GlobalBytes: BatchGlobalBytesLimit, GlobalBatches: BatchGlobalCountLimit,
		DailyCriticalBytes: BatchCriticalDayBytes, DailyCriticalBatches: BatchCriticalDayCount,
		GlobalCriticalBytes: BatchCriticalAllBytes, GlobalCriticalBatches: BatchCriticalAllCount,
	}
}

func EventBatchKey(date, id string) string { return BatchPrefix + date + "/" + id + ".json" }

func VerifyEventBatch(batch EventBatch, key string) error {
	if batch.SchemaVersion != 2 || batch.ID == "" || len(batch.Events) == 0 || len(batch.Events) > BatchMaxEvents ||
		key != EventBatchKey(batch.Date, batch.ID) {
		return storage.ErrCorruptObject
	}
	content, err := json.Marshal(batch.Events)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(hash[:])
	if contentSHA != batch.ContentSHA256 || ID("event-batch", batch.Date, contentSHA) != batch.ID {
		return storage.ErrCorruptObject
	}
	for _, event := range batch.Events {
		if event.ID == "" || event.OccurredAt.IsZero() || EventBatchDate(event) != batch.Date {
			return storage.ErrCorruptObject
		}
	}
	data, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if len(data) > BatchMaxBytes {
		return storage.ErrCorruptObject
	}
	return nil
}

func (log BatchLog) Append(ctx context.Context, events []Event, critical bool) (EventBatch, error) {
	batch, data, reservation, err := buildEventBatch(events, critical)
	if err != nil {
		return EventBatch{}, err
	}
	behind, err := EventDateBehindWatermark(ctx, log.Store, batch.Date)
	if err != nil {
		return EventBatch{}, err
	}
	if behind {
		return EventBatch{}, ErrBatchHistoryExpired
	}
	for _, event := range batch.Events {
		if time.Since(event.OccurredAt) > log.retention() {
			return EventBatch{}, ErrBatchHistoryExpired
		}
	}
	if err := log.reserve(ctx, batch.ID, reservation); err != nil {
		return EventBatch{}, err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return EventBatch{}, err
	}
	key := EventBatchKey(batch.Date, batch.ID)
	entry, err := log.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return rejectBatchIfBehindWatermark(ctx, log.Store, batch, entry)
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return EventBatch{}, err
	}
	stored, _, readErr := inventory.Read[EventBatch](ctx, log.Store, EventBatchKey(batch.Date, batch.ID))
	actual, marshalErr := json.Marshal(stored)
	if readErr == nil && marshalErr == nil && bytes.Equal(actual, data) && stored.ContentSHA256 == batch.ContentSHA256 {
		entry, headErr := log.Store.Head(ctx, key)
		if headErr != nil {
			return EventBatch{}, headErr
		}
		return rejectBatchIfBehindWatermark(ctx, log.Store, batch, entry)
	}
	return EventBatch{}, errors.Join(err, readErr, marshalErr, storage.ErrCorruptObject)
}

func rejectBatchIfBehindWatermark(ctx context.Context, store storage.Store, batch EventBatch, entry storage.Entry) (EventBatch, error) {
	behind, err := EventDateBehindWatermark(ctx, store, batch.Date)
	if err != nil {
		return EventBatch{}, err
	}
	if !behind {
		return batch, nil
	}
	deleteErr := store.DeleteIfMatch(ctx, EventBatchKey(batch.Date, batch.ID), entry.Version)
	if errors.Is(deleteErr, storage.ErrNotFound) {
		deleteErr = nil
	}
	return EventBatch{}, errors.Join(ErrBatchHistoryExpired, deleteErr)
}

func (log BatchLog) reserve(ctx context.Context, id string, reservation EventBatchReservation) error {
	policy := log.Policy
	if policy.DailyBytes == 0 {
		policy = ObservationBatchPolicy()
	}
	rejected := false
	err := inventory.Update(ctx, log.Store, BatchBudgetKey, func(budget *EventBatchBudget) error {
		if budget.Reservations == nil {
			budget.Reservations = map[string]EventBatchReservation{}
		}
		if prior, ok := budget.Reservations[id]; ok {
			if prior != reservation {
				return storage.ErrCorruptObject
			}
			return nil
		}
		cutoff := time.Now().UTC().Add(-log.retention()).Format("20060102")
		for batchID, current := range budget.Reservations {
			if current.Date < cutoff {
				delete(budget.Reservations, batchID)
			}
		}
		var dayBytes, dayCount, globalBytes, globalCount int64
		var dayCriticalBytes, dayCriticalCount, globalCriticalBytes, globalCriticalCount int64
		for _, current := range budget.Reservations {
			globalBytes += current.Bytes
			globalCount++
			if current.Critical {
				globalCriticalBytes += current.Bytes
				globalCriticalCount++
			}
			if current.Date == reservation.Date {
				dayBytes += current.Bytes
				dayCount++
				if current.Critical {
					dayCriticalBytes += current.Bytes
					dayCriticalCount++
				}
			}
		}
		ordinaryDailyBytes := policy.DailyBytes - policy.DailyCriticalBytes
		ordinaryDailyCount := policy.DailyBatches - policy.DailyCriticalBatches
		ordinaryGlobalBytes := policy.GlobalBytes - policy.GlobalCriticalBytes
		ordinaryGlobalCount := policy.GlobalBatches - policy.GlobalCriticalBatches
		if reservation.Critical {
			rejected = dayBytes+reservation.Bytes > policy.DailyBytes || dayCount+1 > policy.DailyBatches ||
				globalBytes+reservation.Bytes > policy.GlobalBytes || globalCount+1 > policy.GlobalBatches
		} else {
			rejected = dayBytes+reservation.Bytes > ordinaryDailyBytes+dayCriticalBytes || dayCount+1 > ordinaryDailyCount+dayCriticalCount ||
				globalBytes+reservation.Bytes > ordinaryGlobalBytes+globalCriticalBytes || globalCount+1 > ordinaryGlobalCount+globalCriticalCount
		}
		if rejected {
			budget.RejectedBatches++
			if reservation.Critical {
				budget.RejectedCritical++
			}
			return nil
		}
		budget.Reservations[id] = reservation
		return nil
	})
	if err != nil {
		return err
	}
	if rejected {
		return ErrHistoryCapacity
	}
	return nil
}

func (log BatchLog) Summary(ctx context.Context, date string) (EventBatchBudgetSummary, error) {
	budget, _, err := inventory.Read[EventBatchBudget](ctx, log.Store, BatchBudgetKey)
	if errors.Is(err, storage.ErrNotFound) {
		budget, err = EventBatchBudget{}, nil
	}
	if err != nil {
		return EventBatchBudgetSummary{}, err
	}
	policy := log.Policy
	if policy.DailyBytes == 0 {
		policy = ObservationBatchPolicy()
	}
	summary := EventBatchBudgetSummary{
		Date: date, RejectedBatches: budget.RejectedBatches, RejectedCritical: budget.RejectedCritical,
		DailyBytesLimit: policy.DailyBytes, DailyBatchesLimit: policy.DailyBatches,
		GlobalBytesLimit: policy.GlobalBytes, GlobalBatchesLimit: policy.GlobalBatches,
		DailyCriticalBytes: policy.DailyCriticalBytes, DailyCriticalBatches: policy.DailyCriticalBatches,
		GlobalCriticalBytes: policy.GlobalCriticalBytes, GlobalCriticalBatches: policy.GlobalCriticalBatches,
	}
	cutoff := time.Now().UTC().Add(-log.retention()).Format("20060102")
	for _, reservation := range budget.Reservations {
		if reservation.Date < cutoff {
			continue
		}
		summary.GlobalBytes += reservation.Bytes
		summary.GlobalBatches++
		if reservation.Date == date {
			summary.DailyBytes += reservation.Bytes
			summary.DailyBatches++
		}
	}
	return summary, nil
}

func buildEventBatch(events []Event, critical bool) (EventBatch, []byte, EventBatchReservation, error) {
	if len(events) == 0 || len(events) > BatchMaxEvents {
		return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
	}
	unique := make([]Event, 0, len(events))
	seen := map[string][]byte{}
	for _, event := range events {
		if event.ID == "" || event.OccurredAt.IsZero() {
			return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return EventBatch{}, nil, EventBatchReservation{}, err
		}
		if prior, ok := seen[event.ID]; ok {
			if !bytes.Equal(prior, encoded) {
				return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
			}
			continue
		}
		seen[event.ID] = encoded
		unique = append(unique, event)
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].OccurredAt.Equal(unique[j].OccurredAt) {
			return unique[i].ID < unique[j].ID
		}
		return unique[i].OccurredAt.Before(unique[j].OccurredAt)
	})
	date := unique[0].OccurredAt.UTC().Format("20060102")
	created := unique[0].OccurredAt.UTC()
	for _, event := range unique {
		if event.OccurredAt.UTC().Format("20060102") != date {
			return EventBatch{}, nil, EventBatchReservation{}, fmt.Errorf("event batch spans UTC partitions: %w", storage.ErrCorruptObject)
		}
		if event.OccurredAt.After(created) {
			created = event.OccurredAt.UTC()
		}
	}
	content, err := json.Marshal(unique)
	if err != nil {
		return EventBatch{}, nil, EventBatchReservation{}, err
	}
	contentHash := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(contentHash[:])
	id := ID("event-batch", date, contentSHA)
	batch := EventBatch{SchemaVersion: 2, ID: id, Date: date, ContentSHA256: contentSHA, CreatedAt: created, Events: unique}
	data, err := json.Marshal(batch)
	if err != nil {
		return EventBatch{}, nil, EventBatchReservation{}, err
	}
	if len(data) > BatchMaxBytes {
		return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
	}
	return batch, data, EventBatchReservation{Date: date, Bytes: int64(len(data)), Critical: critical}, nil
}

func EventBatchDate(event Event) string { return event.OccurredAt.UTC().Format("20060102") }
