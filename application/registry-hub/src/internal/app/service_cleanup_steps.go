package app

import (
	"context"
	"errors"
	"registry-hub/internal/download"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"time"
)

func (s *Service) acquireDownloadCleanup(ctx context.Context, identity, _ string) (context.Context, func(), bool, error) {
	leaseCtx, objectLease, release, err := acquireRenewedLease(ctx, s.tasks.Leases(), "objects/"+identity, "download-cleanup-"+requestID(), objectLeaseTTL)
	if errors.Is(err, lease.ErrHeld) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	guarded := download.WithCleanupGuard(leaseCtx, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, objectLease) })
	return guarded, func() { _ = release() }, true, nil
}

func (s *Service) runDownloadQuotaCleanup(ctx context.Context) {
	last := ""
	s.runPeriodicMaintenance(ctx, "download_quota", func(ctx context.Context) error {
		return s.downloadManager().CleanupOrphanReservationStep(ctx, &last, s.acquireDownloadCleanup)
	})
}

func (s *Service) runStorageOrphanCleanup(ctx context.Context) {
	cleaner, ok := s.store.(interface {
		CleanupOrphansStep(context.Context, time.Duration, string) (string, int, error)
	})
	if !ok {
		return
	}
	token := ""
	s.runPeriodicMaintenance(ctx, "storage_orphans", func(ctx context.Context) error {
		next, _, err := cleaner.CleanupOrphansStep(ctx, time.Hour, token)
		token = next
		return err
	})
}

func (s *Service) runPeriodicMaintenance(ctx context.Context, kind string, step func(context.Context) error) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	delay := time.Second
	next := time.Time{}
	status := backgroundScanStatus{StartedAt: time.Now().UTC()}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(next) {
			continue
		}
		bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
		release, ok := s.backgroundBudget(bounded)
		if ok {
			err := step(bounded)
			if err != nil {
				s.backgroundEvent(kind, "deferred", err)
				next = time.Now().Add(delay + time.Duration(time.Now().UnixNano()%int64(delay/4+1)))
				delay = min(delay*2, 5*time.Minute)
				status.ErrorAt, status.ErrorClass, status.ErrorRecovered, status.Complete, status.NextAttemptAt = time.Now().UTC(), "storage", false, false, next
			} else {
				delay = time.Second
				status.ProgressAt, status.ObservedAt, status.ErrorRecovered, status.Complete = time.Now().UTC(), time.Now().UTC(), true, true
			}
			_ = inventory.Update(bounded, s.store, "background/v1/scans/"+kind+".json", func(value *backgroundScanStatus) error {
				if value.ErrorAt.After(status.ErrorAt) {
					status.ErrorAt, status.ErrorClass = value.ErrorAt, value.ErrorClass
				}
				*value = status
				return nil
			})
			release()
		}
		cancel()
	}
}
