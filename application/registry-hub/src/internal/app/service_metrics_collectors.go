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

func (s *Service) runCacheMetrics(ctx context.Context) {
	p := artifact.StatsCursor{}
	s.runMetricSweep(ctx, "cache", &p.Cursor, accumulatorFor(&p), func(ctx context.Context) (bool, error) { return s.cache.StatsStep(ctx, &p, time.Hour) }, func() { s.metrics.setCacheStats(p.Stats) }, func() { p = artifact.StatsCursor{} })
}

func (s *Service) runIntentMetrics(ctx context.Context) {
	cursor, counts := inventory.Cursor{}, map[string]float64{}
	s.runMetricSweep(ctx, "intents", &cursor, accumulatorFor(&counts), func(ctx context.Context) (bool, error) {
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
	}, func() { cursor = inventory.Cursor{}; counts = map[string]float64{} }, func() map[string]int64 {
		result := map[string]int64{}
		for key, value := range counts {
			result[key] = int64(value)
		}
		return result
	})
}

func (s *Service) runProtectionMetrics(ctx context.Context) {
	cursor := inventory.Cursor{}
	totals := struct{ Counts, Blockers map[string]float64 }{map[string]float64{}, map[string]float64{}}
	s.runMetricSweep(ctx, "protection", &cursor, accumulatorFor(&totals), func(ctx context.Context) (bool, error) {
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
				totals.Counts[state]++
				totals.Blockers[reason]++
			}
			cursor.Advance()
		}
		return cursor.Done, nil
	}, func() {
		s.metrics.protectionSessions.Reset()
		s.metrics.gcBlockers.Reset()
		for _, state := range []string{"protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required", "other"} {
			s.metrics.protectionSessions.WithLabelValues(state).Set(totals.Counts[state])
		}
		for _, reason := range []string{"executing", "unanchored", "cancellation", "unknown", "anchored", "other"} {
			s.metrics.gcBlockers.WithLabelValues(reason).Set(totals.Blockers[reason])
		}
		s.publishProtectionQuotas()
	}, func() {
		cursor = inventory.Cursor{}
		totals.Counts, totals.Blockers = map[string]float64{}, map[string]float64{}
	})
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
