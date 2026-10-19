package app

import (
	"context"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func (s *Service) protectCacheObject(ctx context.Context, authority, repository, digest string) (bool, error) {
	active, err := s.tasks.Leases().Active(ctx, "objects/"+objectLeaseKey(authority, repository, digest))
	if err != nil {
		return false, err
	}
	if active {
		return true, nil
	}
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 1000)
		if err != nil {
			return false, err
		}
		for _, task := range items {
			if task.Authority != authority || task.Repository != repository || !cacheReferencedByTask(task, digest) {
				continue
			}
			switch task.Status {
			case tasks.Pending, tasks.Running, tasks.RetryWait, tasks.RepairPending:
				return true, nil
			case tasks.Failed, tasks.Cancelled:
				if time.Since(task.UpdatedAt) <= 7*24*time.Hour {
					return true, nil
				}
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func cacheReferencedByTask(task tasks.Task, digest string) bool {
	if task.RootDigest == digest {
		return true
	}
	_, ok := task.Graph[digest]
	return ok
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
			s.metrics.setConcurrency("client", len(s.clientSlots))
			s.metrics.setConcurrency("no_auth", len(s.noAuthSlots))
			s.metrics.setConcurrency("push", len(s.pushSlots))
			s.metrics.setConcurrency("upstream", len(s.upstreamSlots))
			s.metrics.setConcurrency("solidification", len(s.solidificationSlots))
			items, _, err := s.tasks.List(ctx, "", "", 1000)
			if err == nil {
				counts := make(map[tasks.State]int)
				authorityCounts := make(map[string]int)
				errorCounts := make(map[string]int)
				for _, item := range items {
					counts[item.Status]++
					authorityCounts[string(item.Status)+"\x00"+taskAuthorityClass(item.Authority)]++
					if item.ErrorClass != "" {
						errorCounts[stableTaskErrorClass(item.ErrorClass)]++
					}
				}
				s.metrics.taskAuthorities.Reset()
				s.metrics.taskErrors.Reset()
				for _, state := range []tasks.State{tasks.Pending, tasks.Running, tasks.RetryWait, tasks.Solidified, tasks.Failed, tasks.Cancelled, tasks.RepairPending} {
					s.metrics.tasks.WithLabelValues(string(state)).Set(float64(counts[state]))
					for _, authorityClass := range []string{"docker", "public", "local"} {
						s.metrics.taskAuthorities.WithLabelValues(string(state), authorityClass).Set(float64(authorityCounts[string(state)+"\x00"+authorityClass]))
					}
				}
				for _, class := range []string{"upstream", "manifest_invalid", "storage", "referrer_index", "lease", "other"} {
					s.metrics.taskErrors.WithLabelValues(class).Set(float64(errorCounts[class]))
				}
			}
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
				s.ready.Store(true)
			}
		}
	}
}

func (s *Service) refreshCacheMetrics(ctx context.Context) {
	stats, err := s.cache.Stats(ctx, time.Hour)
	if err == nil {
		s.metrics.setCacheStats(stats)
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
			if err := s.cleanupPartialDownloads(ctx, utcNow); err != nil {
				s.logger.Warn("partial download cleanup failed", "error_class", "storage")
			}
			if _, err := s.uploads.CleanupExpired(ctx, s.gate, 100); err != nil {
				s.logger.Warn("expired upload cleanup failed", "error_class", "distribution")
			}
			if _, err := s.cache.Cleanup(ctx, utcNow, 1000); err != nil {
				s.logger.Warn("cache cleanup failed", "error_class", "storage")
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
