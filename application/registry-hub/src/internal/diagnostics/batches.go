package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const (
	BatchPrefix           = "diagnostics/v2/event-batches/"
	BatchBudgetKey        = "diagnostics/v2/event-batch-budget.json"
	BatchMaxEvents        = 128
	BatchMaxBytes         = 64 << 10
	BatchDailyBytesLimit  = int64(4 << 20)
	BatchDailyCountLimit  = int64(128)
	BatchGlobalBytesLimit = int64(32 << 20)
	BatchGlobalCountLimit = int64(1024)
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
	holder, err := acquireEventDayLease(ctx, log.Store, batch.Date)
	if err != nil {
		return EventBatch{}, err
	}
	defer releaseEventDayLease(ctx, log.Store, holder)
	return log.appendLocked(ctx, batch, data, reservation)
}

func (log BatchLog) appendLocked(ctx context.Context, batch EventBatch, data []byte, reservation EventBatchReservation) (EventBatch, error) {
	behind, err := eventBatchBehindWatermark(ctx, log.Store, batch)
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
		ranges, err := readEventDaySeal(ctx, store, batch.Date)
		if err != nil {
			return EventBatch{}, err
		}
		sealedCount := 0
		for _, event := range batch.Events {
			if eventInSealedRanges(event.OccurredAt, ranges) {
				sealedCount++
			}
		}
		if sealedCount == 0 {
			return batch, nil
		}
		if sealedCount < len(batch.Events) {
			return EventBatch{}, ErrBatchHistoryExpired
		}
		behind = true
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

func eventBatchBehindWatermark(ctx context.Context, store storage.Store, batch EventBatch) (bool, error) {
	behind, err := EventDateBehindWatermark(ctx, store, batch.Date)
	if err != nil || behind {
		return behind, err
	}
	ranges, err := readEventDaySeal(ctx, store, batch.Date)
	if err != nil {
		return false, err
	}
	for _, event := range batch.Events {
		if eventInSealedRanges(event.OccurredAt, ranges) {
			return true, nil
		}
	}
	return false, nil
}

func EventBatchDate(event Event) string { return event.OccurredAt.UTC().Format("20060102") }
