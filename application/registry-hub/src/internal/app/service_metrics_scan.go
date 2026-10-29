package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) runMetricSweep(ctx context.Context, kind string, cursor *inventory.Cursor, step func(context.Context) (bool, error), publish, reset func()) {
	status := backgroundScanStatus{StartedAt: time.Now().UTC()}
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	previous, _, readErr := inventory.Read[backgroundScanStatus](initial, s.store, "background/v1/statistics/"+kind+".json")
	initialCancel()
	if readErr == nil {
		status.ErrorAt, status.ErrorEntity, status.ErrorClass, status.ErrorRecovered = previous.ErrorAt, previous.ErrorEntity, previous.ErrorClass, previous.ErrorRecovered
	}
	invalid := false
	nextRound, lastSave := time.Time{}, time.Time{}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(nextRound) {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		release, ok := s.backgroundBudget(bounded)
		if !ok {
			cancel()
			continue
		}
		done, err := false, error(nil)
		for unit := 0; unit < 32; unit++ {
			if deadline, ok := bounded.Deadline(); ok && time.Until(deadline) < time.Second {
				break
			}
			done, err = step(bounded)
			if done || err != nil {
				break
			}
		}
		if err != nil {
			invalid = true
			status.Complete = false
			status.ErrorAt, status.ErrorClass, status.ErrorRecovered = time.Now().UTC(), "storage", false
			if cursor.Offset < len(cursor.Entries) {
				status.ErrorEntity = cursor.Entries[cursor.Offset].Key
				cursor.Advance()
				done = cursor.Done
			}
			status.NextAttemptAt = time.Now().UTC().Add(5 * time.Second)
			nextRound = status.NextAttemptAt
			s.backgroundEvent("statistics_"+kind, "scan_failed", err)
		} else {
			status.ProgressAt = time.Now().UTC()
		}
		if done {
			if !invalid {
				publish()
				status.ObservedAt, status.Complete, status.ErrorRecovered = status.StartedAt, true, true
			}
			if invalid {
				status.Complete = false
			}
			reset()
			invalid = false
			status.StartedAt = time.Now().UTC()
			nextRound = time.Now().Add(15 * time.Second)
		}
		if done || time.Since(lastSave) > 5*time.Second {
			_ = inventory.Update(bounded, s.store, "background/v1/statistics/"+kind+".json", func(value *backgroundScanStatus) error { *value = status; return nil })
			lastSave = time.Now()
		}
		release()
		cancel()
	}
}

func (s *Service) runCacheMetrics(ctx context.Context) {
	p := artifact.StatsCursor{}
	s.runMetricSweep(ctx, "cache", &p.Cursor, func(ctx context.Context) (bool, error) { return s.cache.StatsStep(ctx, &p, time.Hour) }, func() { s.metrics.setCacheStats(p.Stats) }, func() { p = artifact.StatsCursor{} })
}

func (s *Service) runIntentMetrics(ctx context.Context) {
	cursor, counts := inventory.Cursor{}, map[string]float64{}
	s.runMetricSweep(ctx, "intents", &cursor, func(ctx context.Context) (bool, error) {
		entry, ok, err := cursor.Peek(ctx, s.store, "solidification/blobs/intents/", 32)
		if err != nil {
			return false, err
		}
		if ok {
			intent, _, err := inventory.Read[tasks.BlobIntent](ctx, s.store, entry.Key)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, err
			}
			if err == nil {
				counts[stableBlobIntentState(intent.State)]++
			}
			cursor.Advance()
		}
		return cursor.Done, nil
	}, func() {
		s.metrics.blobIntentLimit.Set(float64(s.cfg.Solidification.MaxBlobIntents))
		s.metrics.blobIntents.Reset()
		for _, state := range blobIntentMetricStates {
			s.metrics.blobIntents.WithLabelValues(state).Set(counts[state])
		}
	}, func() { cursor = inventory.Cursor{}; counts = map[string]float64{} })
}

func (s *Service) runProtectionMetrics(ctx context.Context) {
	cursor, counts, blockers := inventory.Cursor{}, map[string]float64{}, map[string]float64{}
	s.runMetricSweep(ctx, "protection", &cursor, func(ctx context.Context) (bool, error) {
		entry, ok, err := cursor.Peek(ctx, s.store, "system/solidification/sessions/", 32)
		if err != nil {
			return false, err
		}
		if ok {
			item, _, err := inventory.Read[protectionSessionRecord](ctx, s.store, entry.Key)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, err
			}
			if err == nil && containsString(s.gate.State().SessionIDs, item.ID) {
				state, reason := protectionMetricLabels(item)
				counts[state]++
				blockers[reason]++
			}
			cursor.Advance()
		}
		return cursor.Done, nil
	}, func() {
		s.metrics.protectionSessions.Reset()
		s.metrics.gcBlockers.Reset()
		for _, state := range []string{"protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required", "other"} {
			s.metrics.protectionSessions.WithLabelValues(state).Set(counts[state])
		}
		for _, reason := range []string{"executing", "unanchored", "cancellation", "unknown", "anchored", "other"} {
			s.metrics.gcBlockers.WithLabelValues(reason).Set(blockers[reason])
		}
		s.publishProtectionQuotas()
	}, func() { cursor = inventory.Cursor{}; counts, blockers = map[string]float64{}, map[string]float64{} })
}

func protectionMetricLabels(item protectionSessionRecord) (string, string) {
	state, reason := item.State, "other"
	switch state {
	case "protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required":
	default:
		state = "other"
	}
	switch {
	case item.UncertainRemoteResult || item.RecoveryReason != "" || item.State == "recovery_required":
		reason = "unknown"
	case item.State == "anchored":
		reason = "anchored"
	case item.State == "stop_requested" || item.State == "quiescing" || item.State == "quiesced":
		reason = "cancellation"
	case item.MayContainUnanchored:
		reason = "unanchored"
	case item.State == "protected" || item.State == "anchoring":
		reason = "executing"
	}
	return state, reason
}

func (s *Service) publishProtectionQuotas() {
	gate := s.gate.State()
	s.metrics.protectionReserved.WithLabelValues("new").Set(float64(gate.ReservedDistributionBytes))
	s.metrics.protectionReserved.WithLabelValues("legacy").Set(float64(gate.LegacyGrandfatheredBytes))
	s.metrics.protectionLimit.Set(float64(s.cfg.Solidification.MaxUnanchoredDistributionBytes))
	s.metrics.protectedCacheReserved.Set(float64(gate.ProtectedCacheBytes))
	s.metrics.protectedCacheLimit.Set(float64(protectedCacheLimit(s.cfg)))
	if gate.RecoveryBarrier {
		s.metrics.recoveryBarrier.Set(1)
	} else {
		s.metrics.recoveryBarrier.Set(0)
	}
}
