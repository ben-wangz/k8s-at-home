package app

import (
	"context"
	"time"
)

func (s *Service) refreshMetrics(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		s.metrics.setGateState(s.gate.State().State)
		s.publishProtectionQuotas()
		s.metrics.setConcurrency("client", len(s.clientSlots))
		s.metrics.setConcurrency("no_auth", len(s.noAuthSlots))
		s.metrics.setConcurrency("push", len(s.pushSlots))
		s.metrics.setConcurrency("upstream", len(s.upstreamSlots))
		s.metrics.setConcurrency("solidification", len(s.solidificationSlots))
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		s.refreshDownloadAndReplayMetrics(bounded)
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) refreshBlobIntentMetrics(ctx context.Context) {
	s.metrics.blobIntentLimit.Set(float64(s.cfg.Solidification.MaxBlobIntents))
	counts := make(map[string]float64, len(blobIntentMetricStates))
	token := ""
	for {
		intents, next, err := s.records.ListBlobIntents(ctx, token, 1000)
		if err != nil {
			return
		}
		for _, intent := range intents {
			counts[stableBlobIntentState(intent.State)]++
		}
		if next == "" {
			break
		}
		token = next
	}
	s.metrics.blobIntents.Reset()
	for _, state := range blobIntentMetricStates {
		s.metrics.blobIntents.WithLabelValues(state).Set(counts[state])
	}
}

func (s *Service) refreshProtectionMetrics(ctx context.Context) {
	counts := make(map[string]float64)
	blockers := make(map[string]float64)
	token := ""
	for {
		items, next, err := s.gate.ListProtectionSessions(ctx, token, 100)
		if err != nil {
			return
		}
		for _, item := range items {
			state := item.State
			switch state {
			case "protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required":
			default:
				state = "other"
			}
			counts[state]++
			reason := "other"
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
			blockers[reason]++
		}
		if next == "" {
			break
		}
		token = next
	}
	s.metrics.protectionSessions.Reset()
	for _, state := range []string{"protected", "anchoring", "stop_requested", "quiescing", "quiesced", "anchored", "recovery_required", "other"} {
		s.metrics.protectionSessions.WithLabelValues(state).Set(counts[state])
	}
	s.metrics.gcBlockers.Reset()
	for _, reason := range []string{"executing", "unanchored", "cancellation", "unknown", "anchored", "other"} {
		s.metrics.gcBlockers.WithLabelValues(reason).Set(blockers[reason])
	}
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
