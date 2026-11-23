package diagnostics

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

const EventHistoryWatermarkKey = Prefix + "event-history-watermark.json"

type EventHistoryWatermark struct {
	SchemaVersion int       `json:"schemaVersion"`
	BeforeDate    string    `json:"beforeDate"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func EventBehindWatermark(ctx context.Context, store storage.Store, event Event) (bool, error) {
	return EventDateBehindWatermark(ctx, store, EventBatchDate(event))
}

func EventDateBehindWatermark(ctx context.Context, store storage.Store, date string) (bool, error) {
	watermark, _, err := inventory.Read[EventHistoryWatermark](ctx, store, EventHistoryWatermarkKey)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if watermark.SchemaVersion != 1 || !validEventDate(watermark.BeforeDate) {
		return false, storage.ErrCorruptObject
	}
	return date < watermark.BeforeDate, nil
}

func AdvanceEventHistoryWatermark(ctx context.Context, store storage.Store, beforeDate string) error {
	if !validEventDate(beforeDate) {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, store, EventHistoryWatermarkKey, func(current *EventHistoryWatermark) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || !validEventDate(current.BeforeDate)) {
			return storage.ErrCorruptObject
		}
		if current.BeforeDate >= beforeDate {
			return nil
		}
		*current = EventHistoryWatermark{SchemaVersion: 1, BeforeDate: beforeDate, UpdatedAt: time.Now().UTC()}
		return nil
	})
}

func validEventDate(value string) bool {
	parsed, err := time.Parse("20060102", value)
	return err == nil && parsed.UTC().Format("20060102") == value
}
