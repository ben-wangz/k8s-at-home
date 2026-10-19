package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"time"
)

func (s *Service) cleanupPartialDownloads(ctx context.Context, now time.Time) error {
	manager := s.downloadManager()
	removed, err := manager.Cleanup(ctx, now, s.cfg.Limits.DownloadPartialTTL, 100, func(ctx context.Context, identity, _ string) (context.Context, func(), bool, error) {
		leaseCtx, _, release, err := acquireRenewedLease(ctx, s.tasks.Leases(), "objects/"+identity, "download-cleanup-"+requestID(), objectLeaseTTL)
		if errors.Is(err, lease.ErrHeld) {
			return nil, nil, false, nil
		}
		if err != nil {
			return nil, nil, false, err
		}
		return leaseCtx, func() { _ = release() }, true, nil
	})
	if err != nil {
		return err
	}
	if removed > 0 {
		s.metrics.recordEvent("download_partial_cleanup")
	}
	return nil
}
