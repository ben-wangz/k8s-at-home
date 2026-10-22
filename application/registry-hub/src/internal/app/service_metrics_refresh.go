package app

import (
	"context"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) refreshMetrics(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	cacheTicker := time.NewTicker(time.Hour)
	defer cacheTicker.Stop()
	healthTicker := time.NewTicker(60 * time.Second)
	defer healthTicker.Stop()
	s.refreshCacheMetrics(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.metrics.setGateState(s.gate.State().State)
			s.refreshProtectionMetrics(ctx)
			s.metrics.setConcurrency("client", len(s.clientSlots))
			s.metrics.setConcurrency("no_auth", len(s.noAuthSlots))
			s.metrics.setConcurrency("push", len(s.pushSlots))
			s.metrics.setConcurrency("upstream", len(s.upstreamSlots))
			s.metrics.setConcurrency("solidification", len(s.solidificationSlots))
			counts := make(map[tasks.State]int)
			authorityCounts := make(map[string]int)
			errorCounts := make(map[string]int)
			kindCounts := make(map[string]int)
			waitCounts := make(map[string]int)
			retryCounts := make(map[string]float64)
			cancelCounts := make(map[string]int)
			var runnable, waiting, recoveryUnknown float64
			token := ""
			for {
				items, next, err := s.tasks.List(ctx, "", token, 1000)
				if err != nil {
					counts = nil
					break
				}
				for _, item := range items {
					counts[item.Status]++
					authorityCounts[string(item.Status)+"\x00"+taskAuthorityClass(item.Authority)]++
					kind := stableTaskKind(item.Kind)
					key := kind + "\x00" + string(item.Status)
					kindCounts[key]++
					if item.RetryCount > 0 {
						retryCounts[key] += float64(item.RetryCount)
					}
					switch item.Status {
					case tasks.Pending, tasks.Running, tasks.RetryWait:
						if item.Phase == "solidification" {
							runnable++
						}
					case tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal:
						waiting++
						waitCounts[stableTaskWaitReason(item.WaitReason)]++
					}
					if item.CancelRequested {
						cancelCounts[stableTaskCancelPhase(item.CancelPhase)]++
					}
					if item.RecoveryReason != "" {
						recoveryUnknown++
					}
					if item.ErrorClass != "" {
						errorCounts[stableTaskErrorClass(item.ErrorClass)]++
					}
				}
				if next == "" {
					break
				}
				token = next
			}
			if counts != nil {
				s.metrics.taskKinds.Reset()
				s.metrics.taskAuthorities.Reset()
				s.metrics.taskErrors.Reset()
				s.metrics.taskWaitReasons.Reset()
				s.metrics.taskRetries.Reset()
				s.metrics.taskCancelPhases.Reset()
				for _, state := range []tasks.State{tasks.Pending, tasks.Running, tasks.RetryWait, tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal, tasks.Solidified, tasks.Failed, tasks.Cancelled, tasks.RepairPending} {
					s.metrics.tasks.WithLabelValues(string(state)).Set(float64(counts[state]))
					for _, kind := range []string{"root", "blob", "other"} {
						key := kind + "\x00" + string(state)
						s.metrics.taskKinds.WithLabelValues(kind, string(state)).Set(float64(kindCounts[key]))
						s.metrics.taskRetries.WithLabelValues(kind, string(state)).Set(retryCounts[key])
					}
					for _, authorityClass := range []string{"docker", "public", "local"} {
						s.metrics.taskAuthorities.WithLabelValues(string(state), authorityClass).Set(float64(authorityCounts[string(state)+"\x00"+authorityClass]))
					}
				}
				for _, reason := range taskWaitReasonLabels {
					s.metrics.taskWaitReasons.WithLabelValues(reason).Set(float64(waitCounts[reason]))
				}
				for _, phase := range []string{"stop_requested", "quiesced", "other"} {
					s.metrics.taskCancelPhases.WithLabelValues(phase).Set(float64(cancelCounts[phase]))
				}
				for _, class := range []string{"upstream", "manifest_invalid", "storage", "referrer_index", "lease", "other"} {
					s.metrics.taskErrors.WithLabelValues(class).Set(float64(errorCounts[class]))
				}
				s.metrics.taskRunnable.Set(runnable)
				s.metrics.taskWaiting.Set(waiting)
				s.metrics.recoveryUnknown.Set(recoveryUnknown)
			}
			s.refreshBlobIntentMetrics(ctx)
			s.refreshDownloadAndReplayMetrics(ctx)
		case <-cacheTicker.C:
			s.refreshCacheMetrics(ctx)
		case <-healthTicker.C:
			probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			storageErr := storage.Probe(probeCtx, s.store)
			registryErr := s.registry.Ping(probeCtx)
			cancel()
			if storageErr != nil {
				s.ready.Store(false)
				s.logger.Warn("storage health probe failed", "error_class", "storage")
			} else if registryErr != nil {
				s.ready.Store(false)
				s.logger.Warn("internal Registry health probe failed", "error_class", "distribution")
			} else {
				s.ready.Store(!s.gate.State().RecoveryBarrier)
			}
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
