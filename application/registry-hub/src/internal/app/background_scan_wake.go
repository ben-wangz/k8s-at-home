package app

import (
	"strings"
	"time"
)

const backgroundScanWakeDelay = 5 * time.Second

func advanceScanWake(deadline, retry, now time.Time) time.Time {
	if !deadline.After(now) {
		return deadline
	}
	wake := now.Add(backgroundScanWakeDelay)
	if retry.After(wake) {
		wake = retry
	}
	if wake.Before(deadline) {
		return wake
	}
	return deadline
}

func newBackgroundScanWakeChannels() map[string]chan struct{} {
	channels := make(map[string]chan struct{})
	for _, kind := range []string{
		"source_tasks", "source_downloads", "source_cache", "source_demands", "task_reconciliation", "activation",
		"statistics_tasks", "statistics_cache", "statistics_intents", "statistics_protection",
		"history_tasks", "history_intents", "history_work", "history_receipts", "history_downloads", "history_uploads", "history_upload_receipts",
	} {
		channels[kind] = make(chan struct{}, 1)
	}
	return channels
}

func (s *Service) signalBackgroundScans(key string) {
	switch {
	case strings.HasPrefix(key, "tasks/state/"):
		s.signalBackgroundScan("source_tasks")
		s.signalBackgroundScan("task_reconciliation")
		s.signalBackgroundScan("statistics_tasks")
		s.signalBackgroundScan("history_tasks")
	case strings.HasPrefix(key, "downloads/v1/"):
		s.signalBackgroundScan("source_downloads")
		s.signalBackgroundScan("history_downloads")
	case strings.HasPrefix(key, "cache/objects/"):
		s.signalBackgroundScan("source_cache")
		s.signalBackgroundScan("statistics_cache")
	case strings.HasPrefix(key, "solidification/blobs/owners/"):
		s.signalBackgroundScan("source_demands")
	case strings.HasPrefix(key, "solidification/blobs/intents/"):
		s.signalBackgroundScan("activation")
		s.signalBackgroundScan("statistics_intents")
		s.signalBackgroundScan("history_intents")
	case strings.HasPrefix(key, "system/solidification/sessions/"):
		s.signalBackgroundScan("statistics_protection")
	case strings.HasPrefix(key, "background/v1/jobs/"):
		s.signalBackgroundScan("history_work")
	case strings.HasPrefix(key, "background/v1/receipts/"):
		s.signalBackgroundScan("history_receipts")
	case strings.HasPrefix(key, "uploads/sessions/"):
		s.signalBackgroundScan("history_uploads")
	case strings.HasPrefix(key, "diagnostics/v1/upload-receipts/"):
		s.signalBackgroundScan("history_upload_receipts")
	}
}

func (s *Service) signalBackgroundScan(kind string) {
	if channel := s.backgroundWake[kind]; channel != nil {
		select {
		case channel <- struct{}{}:
		default:
		}
	}
}

func idleScanDelay(base, maximum time.Duration, idleRounds int) time.Duration {
	delay := base
	for round := 1; round < idleRounds && delay < maximum; round++ {
		delay = min(delay*2, maximum)
	}
	return min(delay, maximum)
}

func sourceSweepNextDelay(kind string, idleRounds int, retry bool) time.Duration {
	if retry || idleRounds < 1 {
		return 15 * time.Second
	}
	maximum := time.Minute
	if strings.HasPrefix(kind, "history_") {
		maximum = 5 * time.Minute
	}
	return idleScanDelay(15*time.Second, maximum, idleRounds)
}

func metricSweepNextDelay(idleRounds int) time.Duration {
	if idleRounds < 1 {
		return 15 * time.Second
	}
	return idleScanDelay(15*time.Second, 2*time.Minute, idleRounds)
}
