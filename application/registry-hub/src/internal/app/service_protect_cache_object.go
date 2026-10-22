package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func (s *Service) protectCacheObject(ctx context.Context, authority, repository, digest string) (bool, error) {
	if !s.cacheProtectionReady.Load() {
		return true, nil
	}
	active, err := s.tasks.Leases().Active(ctx, "objects/"+objectLeaseKey(authority, repository, digest))
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	intent, intentErr := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: authority, Repository: repository, Digest: digest})
	if intentErr != nil && !errors.Is(intentErr, storage.ErrNotFound) {
		return false, intentErr
	}
	if intentErr == nil {
		switch intent.State {
		case tasks.IntentWaitingCache, tasks.IntentWaitingQuota, tasks.IntentReady, tasks.IntentRunning, tasks.IntentRetryWait:
			return true, nil
		}
	}
	token := ""
	for {
		references, next, err := s.records.ListCacheProtectionReferences(ctx, digest, token, 500)
		if err != nil {
			return false, err
		}
		for _, reference := range references {
			if reference.Authority != authority || reference.Repository != repository {
				continue
			}
			task, err := s.tasks.Get(ctx, reference.TaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if task.Kind != tasks.RootKind {
				continue
			}
			if !activeTaskNeedsCache(task) && !task.GCProtected {
				continue
			}
			if task.RootDigest == digest && !task.RootPublished {
				return true, nil
			}
			if node, nodeErr := s.records.GetGraphNode(ctx, task.ID, tasks.GraphManifest, digest); nodeErr == nil {
				if node.LocalState != "anchored" {
					return true, nil
				}
				continue
			} else if !errors.Is(nodeErr, storage.ErrNotFound) {
				return false, nodeErr
			}
			if _, nodeErr := s.records.GetGraphNode(ctx, task.ID, tasks.GraphBlob, digest); nodeErr == nil {
				if intentErr != nil || intent.State != tasks.IntentVerified && intent.State != tasks.IntentAnchored {
					return true, nil
				}
				continue
			} else if !errors.Is(nodeErr, storage.ErrNotFound) {
				return false, nodeErr
			}
			if _, exists := task.Graph[digest]; exists && (intentErr != nil || intent.State != tasks.IntentVerified && intent.State != tasks.IntentAnchored) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func activeTaskNeedsCache(task tasks.Task) bool {
	switch task.Status {
	case tasks.Pending, tasks.Running, tasks.RetryWait, tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal, tasks.RepairPending:
		return true
	default:
		return false
	}
}

func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.BeginShutdown()
		s.tasks.Close()
		if s.gc != nil {
			s.gc.Close()
		}
		s.background.Wait()
		s.upstream.Close()
		s.registry.Close()
		_ = s.store.Close()
	})
}

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

func (s *Service) refreshCacheMetrics(ctx context.Context) {
	stats, err := s.cache.Stats(ctx, time.Hour)
	if err == nil {
		s.metrics.setCacheStats(stats)
	}
}

func (s *Service) refreshDownloadAndReplayMetrics(ctx context.Context) {
	if reserved, sessions, err := s.downloadManager().Usage(ctx); err == nil {
		s.metrics.partialReserved.Set(float64(reserved))
		s.metrics.partialSessions.Set(float64(sessions))
		s.metrics.partialLimit.Set(float64(s.cfg.Limits.DownloadMaxBytes))
	}
	if provider, ok := s.store.(interface{ ReplayUsage() (int64, int64, int) }); ok {
		memory, disk, active := provider.ReplayUsage()
		s.metrics.s3ReplayMemory.Set(float64(memory))
		s.metrics.s3ReplayDisk.Set(float64(disk))
		s.metrics.s3ReplayActive.Set(float64(active))
	} else {
		s.metrics.s3ReplayMemory.Set(0)
		s.metrics.s3ReplayDisk.Set(0)
		s.metrics.s3ReplayActive.Set(0)
	}
}

func taskAuthorityClass(authority string) string {
	if authority == "" {
		return "local"
	}
	if strings.EqualFold(authority, "docker.io") {
		return "docker"
	}
	return "public"
}

func stableTaskErrorClass(class string) string {
	switch class {
	case "upstream", "manifest_invalid", "storage", "referrer_index", "lease":
		return class
	default:
		return "other"
	}
}

func stableTaskKind(kind tasks.Kind) string {
	switch kind {
	case tasks.RootKind:
		return "root"
	case tasks.BlobKind:
		return "blob"
	default:
		return "other"
	}
}

func stableBlobIntentState(state tasks.BlobIntentState) string {
	for _, candidate := range blobIntentMetricStates[:len(blobIntentMetricStates)-1] {
		if string(state) == candidate {
			return candidate
		}
	}
	return "other"
}

func stableTaskWaitReason(reason string) string {
	for _, candidate := range taskWaitReasonLabels[:len(taskWaitReasonLabels)-1] {
		if reason == candidate {
			return reason
		}
	}
	return "other"
}

func stableTaskCancelPhase(phase string) string {
	switch phase {
	case "stop_requested", "quiesced":
		return phase
	default:
		return "other"
	}
}

func (s *Service) runCacheCleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	lastOrphanScan := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			utcNow := now.UTC()
			if _, err := s.tasks.ExpireIdle(ctx, 100); err != nil {
				s.logger.Warn("idle solidification expiry failed", "error_class", "storage")
			}
			if err := s.cleanupPartialDownloads(ctx, utcNow); err != nil {
				s.logger.Warn("partial download cleanup failed", "error_class", "storage")
			}
			if _, err := s.uploads.CleanupExpired(ctx, s.gate, 100); err != nil {
				s.logger.Warn("expired upload cleanup failed", "error_class", "distribution")
			}
			if _, err := s.cache.Cleanup(ctx, utcNow, 1000); err != nil {
				s.logger.Warn("cache cleanup failed", "error_class", "storage")
			}
			if _, err := s.cache.CleanupReaderPins(ctx, 1000); err != nil {
				s.logger.Warn("cache reader pin cleanup failed", "error_class", "storage")
			}
			if lastOrphanScan.IsZero() || utcNow.Sub(lastOrphanScan) >= time.Hour {
				if _, err := s.cache.CleanupOrphans(ctx, utcNow, time.Hour, 1000); err != nil {
					s.logger.Warn("cache orphan scan failed", "error_class", "storage")
				}
				if cleaner, ok := s.store.(interface {
					CleanupOrphans(context.Context, time.Duration, int) (int, error)
				}); ok {
					if _, err := cleaner.CleanupOrphans(ctx, time.Hour, 1000); err != nil {
						s.logger.Warn("storage orphan cleanup failed", "error_class", "storage")
					}
				}
				lastOrphanScan = utcNow
			}
			if _, err := s.tasks.Cleanup(ctx, 30*24*time.Hour, 1000); err != nil {
				s.logger.Warn("task cleanup failed", "error_class", "storage")
			}
		}
	}
}

func (s *Service) acquireSlot(ctx context.Context, slot chan struct{}) (func(), bool) {
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, true
	case <-ctx.Done():
		return nil, false
	default:
		return nil, false
	}
}

func (s *Service) setRepairPending(pending bool) {
	s.solidificationMu.Lock()
	s.repairPending = pending
	s.solidificationMu.Unlock()
	if s.metrics != nil {
		s.metrics.setReferrerRepairPending(pending)
	}
}
