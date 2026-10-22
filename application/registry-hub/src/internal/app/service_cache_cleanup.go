package app

import (
	"context"
	"time"
)

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
