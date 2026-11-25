package diagnostics

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type EventWatermarkProgress struct {
	From    string `json:"from,omitempty"`
	Before  string `json:"before,omitempty"`
	Earlier int    `json:"earlier,omitempty"`
	Later   int    `json:"later,omitempty"`
}

// PublishSealedEventDay merges one fully sealed day into the watermark.
func PublishSealedEventDay(ctx context.Context, store storage.Store, date string) error {
	if !validEventDate(date) {
		return storage.ErrCorruptObject
	}
	sealed, err := eventDayFullySealed(ctx, store, date)
	if err != nil || !sealed {
		return err
	}
	return advanceContiguousSealedWatermark(ctx, store, date, dayAfter(date))
}

func advanceSealedEventHistoryRange(ctx context.Context, store storage.Store, from, to time.Time) error {
	var progress EventWatermarkProgress
	for {
		next, done, err := AdvanceSealedEventHistoryWatermarkStep(ctx, store, from, to, progress)
		if done || err != nil {
			return err
		}
		progress = next
	}
}

// AdvanceSealedEventHistoryWatermarkStep expands a sealed range by at most one neighboring day.
func AdvanceSealedEventHistoryWatermarkStep(ctx context.Context, store storage.Store, from, to time.Time, progress EventWatermarkProgress) (EventWatermarkProgress, bool, error) {
	if progress.From == "" {
		start := from.UTC().Truncate(24 * time.Hour)
		if !start.Equal(from) {
			start = start.Add(24 * time.Hour)
		}
		before := to.UTC().Truncate(24 * time.Hour)
		if !start.Before(before) {
			return progress, true, nil
		}
		progress.From, progress.Before = start.Format("20060102"), before.Format("20060102")
	}
	if progress.Earlier < eventHistoryWatermarkExpansionDays {
		previous, ok := dayBefore(progress.From)
		if !ok {
			return progress, false, storage.ErrCorruptObject
		}
		sealed, err := eventDayFullySealed(ctx, store, previous)
		if err != nil {
			return progress, false, err
		}
		if sealed {
			progress.From, progress.Earlier = previous, progress.Earlier+1
		} else {
			progress.Earlier = eventHistoryWatermarkExpansionDays
		}
		return progress, false, nil
	}
	if progress.Later < eventHistoryWatermarkExpansionDays {
		sealed, err := eventDayFullySealed(ctx, store, progress.Before)
		if err != nil {
			return progress, false, err
		}
		if sealed {
			progress.Before, progress.Later = dayAfter(progress.Before), progress.Later+1
		} else {
			progress.Later = eventHistoryWatermarkExpansionDays
		}
		return progress, false, nil
	}
	err := advanceContiguousSealedWatermark(ctx, store, progress.From, progress.Before)
	return progress, err == nil, err
}

func advanceContiguousSealedWatermark(ctx context.Context, store storage.Store, start, before string) error {
	if !validEventDate(start) || !validEventDate(before) || start >= before {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, store, EventHistoryWatermarkKey, func(current *EventHistoryWatermark) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || !validEventDate(current.BeforeDate) || current.FromDate != "" && !validEventDate(current.FromDate)) {
			return storage.ErrCorruptObject
		}
		if current.SchemaVersion == 1 && current.FromDate == "" {
			if start <= current.BeforeDate && before > current.BeforeDate {
				current.BeforeDate, current.UpdatedAt = before, time.Now().UTC()
			}
			return nil
		}
		if current.SchemaVersion == 1 {
			if before <= current.FromDate || start >= current.BeforeDate {
				if before == current.FromDate {
					current.FromDate, current.UpdatedAt = start, time.Now().UTC()
				} else if start == current.BeforeDate {
					current.BeforeDate, current.UpdatedAt = before, time.Now().UTC()
				}
				return nil
			}
			if start < current.FromDate || before > current.BeforeDate {
				current.FromDate = min(start, current.FromDate)
				current.BeforeDate = max(before, current.BeforeDate)
				current.UpdatedAt = time.Now().UTC()
			}
			return nil
		}
		*current = EventHistoryWatermark{SchemaVersion: 1, FromDate: start, BeforeDate: before, UpdatedAt: time.Now().UTC()}
		return nil
	})
}

func eventDayFullySealed(ctx context.Context, store storage.Store, date string) (bool, error) {
	seal, _, err := inventory.Read[EventHistorySeal](ctx, store, EventHistorySealPrefix+date+".json")
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if seal.SchemaVersion != 1 || seal.Date != date || !validEventDate(date) {
		return false, storage.ErrCorruptObject
	}
	day, err := time.Parse("20060102", date)
	if err != nil {
		return false, storage.ErrCorruptObject
	}
	end := day.Add(24 * time.Hour)
	for _, sealed := range seal.Ranges {
		if !sealed.From.Before(sealed.To) || sealed.From.UTC().Format("20060102") != date || sealed.To.Add(-time.Nanosecond).UTC().Format("20060102") != date {
			return false, storage.ErrCorruptObject
		}
		if !sealed.From.After(day) && !sealed.To.Before(end) {
			return true, nil
		}
	}
	return false, nil
}

func dayBefore(date string) (string, bool) {
	value, err := time.Parse("20060102", date)
	if err != nil {
		return "", false
	}
	return value.Add(-24 * time.Hour).Format("20060102"), true
}

func dayAfter(date string) string {
	value, err := time.Parse("20060102", date)
	if err != nil {
		return ""
	}
	return value.Add(24 * time.Hour).Format("20060102")
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func AdvanceEventHistoryWatermark(ctx context.Context, store storage.Store, beforeDate string) error {
	if !validEventDate(beforeDate) {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, store, EventHistoryWatermarkKey, func(current *EventHistoryWatermark) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || !validEventDate(current.BeforeDate) ||
			current.FromDate != "" && !validEventDate(current.FromDate)) {
			return storage.ErrCorruptObject
		}
		if current.SchemaVersion == 1 && current.FromDate != "" {
			if current.BeforeDate >= beforeDate {
				return nil
			}
			if current.BeforeDate != beforeDate && dayAfter(current.BeforeDate) != beforeDate {
				return storage.ErrConditionFailed
			}
			current.BeforeDate, current.UpdatedAt = beforeDate, time.Now().UTC()
			return nil
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
