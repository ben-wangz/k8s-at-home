package diagnostics

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sort"
	"time"
)

func EventBehindWatermark(ctx context.Context, store storage.Store, event Event) (bool, error) {
	behind, err := EventDateBehindWatermark(ctx, store, EventBatchDate(event))
	if err != nil || behind {
		return behind, err
	}
	return EventTimeSealed(ctx, store, event.OccurredAt.UTC())
}

func EventTimeSealed(ctx context.Context, store storage.Store, at time.Time) (bool, error) {
	date := at.UTC().Format("20060102")
	ranges, err := readEventDaySeal(ctx, store, date)
	return eventInSealedRanges(at, ranges), err
}

func readEventDaySeal(ctx context.Context, store storage.Store, date string) ([]EventTimeRange, error) {
	seal, _, err := inventory.Read[EventHistorySeal](ctx, store, EventHistorySealPrefix+date+".json")
	if errors.Is(err, storage.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if seal.SchemaVersion != 1 || seal.Date != date {
		return nil, storage.ErrCorruptObject
	}
	for _, sealed := range seal.Ranges {
		if !sealed.From.Before(sealed.To) || sealed.From.UTC().Format("20060102") != date ||
			sealed.To.Add(-time.Nanosecond).UTC().Format("20060102") != date {
			return nil, storage.ErrCorruptObject
		}
	}
	return seal.Ranges, nil
}

func eventInSealedRanges(at time.Time, ranges []EventTimeRange) bool {
	for _, sealed := range ranges {
		if !at.Before(sealed.From) && at.Before(sealed.To) {
			return true
		}
	}
	return false
}

func SealEventHistoryRange(ctx context.Context, store storage.Store, from, to time.Time) error {
	from, to = from.UTC(), to.UTC()
	if from.IsZero() || !from.Before(to) {
		return storage.ErrCorruptObject
	}
	for date := from.Truncate(24 * time.Hour); date.Before(to); date = date.Add(24 * time.Hour) {
		if err := SealEventHistoryDay(ctx, store, maxTime(from, date), minTime(to, date.Add(24*time.Hour))); err != nil {
			return err
		}
	}
	return advanceSealedEventHistoryRange(ctx, store, from, to)
}

func SealEventHistoryDay(ctx context.Context, store storage.Store, from, to time.Time) error {
	date := from.UTC().Format("20060102")
	if from.IsZero() || !from.Before(to) || to.Add(-time.Nanosecond).UTC().Format("20060102") != date {
		return storage.ErrCorruptObject
	}
	ranges, err := readEventDaySeal(ctx, store, date)
	if err != nil || eventRangeSealed(ranges, from, to) {
		return err
	}
	holder, err := acquireEventDayLease(ctx, store, date)
	if err != nil {
		return err
	}
	sealErr := sealEventDayRange(ctx, store, date, EventTimeRange{From: from.UTC(), To: to.UTC()})
	return errors.Join(sealErr, releaseEventDayLease(ctx, store, holder))
}

func SealExpiredEventDay(ctx context.Context, store storage.Store, date string, retention time.Duration, now time.Time) (bool, error) {
	if !validEventDate(date) || retention < 0 {
		return false, storage.ErrCorruptObject
	}
	day, err := time.Parse("20060102", date)
	if err != nil {
		return false, storage.ErrCorruptObject
	}
	if day.Add(24 * time.Hour).Add(retention).After(now.UTC()) {
		return false, nil
	}
	if err := SealEventHistoryDay(ctx, store, day, day.Add(24*time.Hour)); err != nil {
		return false, err
	}
	return true, nil
}

func sealEventDayRange(ctx context.Context, store storage.Store, date string, value EventTimeRange) error {
	if !validEventDate(date) || !value.From.Before(value.To) || value.From.UTC().Format("20060102") != date ||
		value.To.Add(-time.Nanosecond).UTC().Format("20060102") != date {
		return storage.ErrCorruptObject
	}
	return inventory.Update(ctx, store, EventHistorySealPrefix+date+".json", func(current *EventHistorySeal) error {
		if current.SchemaVersion != 0 && (current.SchemaVersion != 1 || current.Date != date) {
			return storage.ErrCorruptObject
		}
		if current.SchemaVersion == 1 && eventRangeSealed(current.Ranges, value.From, value.To) {
			return nil
		}
		current.SchemaVersion, current.Date = 1, date
		current.Ranges = mergeEventTimeRanges(append(current.Ranges, value))
		if len(current.Ranges) > 64 {
			return storage.ErrUnavailable
		}
		current.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func eventRangeSealed(ranges []EventTimeRange, from, to time.Time) bool {
	for _, sealed := range ranges {
		if !from.Before(sealed.From) && !to.After(sealed.To) {
			return true
		}
	}
	return false
}

func mergeEventTimeRanges(ranges []EventTimeRange) []EventTimeRange {
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].From.Before(ranges[j].From) })
	merged := make([]EventTimeRange, 0, len(ranges))
	for _, current := range ranges {
		if len(merged) == 0 || merged[len(merged)-1].To.Before(current.From) {
			merged = append(merged, current)
			continue
		}
		last := &merged[len(merged)-1]
		if current.To.After(last.To) {
			last.To = current.To
		}
	}
	return merged
}
