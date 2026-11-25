package diagnostics

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const EventHistoryWatermarkKey = Prefix + "event-history-watermark.json"
const EventHistorySealPrefix = "diagnostics/v2/event-history-seals/"
const eventHistoryWatermarkExpansionDays = 7

type EventHistoryWatermark struct {
	SchemaVersion int       `json:"schemaVersion"`
	FromDate      string    `json:"fromDate,omitempty"`
	BeforeDate    string    `json:"beforeDate"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type EventHistorySeal struct {
	SchemaVersion int              `json:"schemaVersion"`
	Date          string           `json:"date"`
	Ranges        []EventTimeRange `json:"ranges"`
	UpdatedAt     time.Time        `json:"updatedAt"`
}

type EventTimeRange struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

func EventDateBehindWatermark(ctx context.Context, store storage.Store, date string) (bool, error) {
	if !validEventDate(date) {
		return false, storage.ErrCorruptObject
	}
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
	if watermark.FromDate == "" {
		return date < watermark.BeforeDate, nil
	}
	if !validEventDate(watermark.FromDate) || watermark.FromDate > watermark.BeforeDate {
		return false, storage.ErrCorruptObject
	}
	return date >= watermark.FromDate && date < watermark.BeforeDate, nil
}
