package app

import (
	"strings"
	"time"
)

const scanScheduleTolerance = 90 * time.Second

func scanMaximumInterval(kind string) time.Duration {
	switch {
	case kind == "storage_inventory":
		return storageInventoryRescanInterval
	case strings.HasPrefix(kind, "statistics_"):
		return metricSweepNextDelay(100)
	case strings.HasPrefix(kind, "history_"):
		return sourceSweepNextDelay(kind, 100, false)
	default:
		return time.Minute
	}
}

func applyScanFreshness(value *backgroundScanStatus, kind string, now time.Time) {
	lastSuccess := value.LastSuccessAt
	if lastSuccess.IsZero() && value.Complete {
		lastSuccess = value.ObservedAt
	}
	value.Stale = lastSuccess.IsZero()
	if !lastSuccess.IsZero() {
		age := max(int64(0), int64(now.Sub(lastSuccess).Seconds()))
		value.AgeSeconds = &age
		value.Stale = now.Sub(lastSuccess) > scanMaximumInterval(kind)+scanScheduleTolerance
	}
	value.ScheduleOverdue = !value.NextScanAt.IsZero() && now.After(value.NextScanAt.Add(scanScheduleTolerance))
	value.Failed = !value.ErrorAt.IsZero() && !value.ErrorRecovered && value.ErrorAt.After(value.LastSuccessAt)
}
