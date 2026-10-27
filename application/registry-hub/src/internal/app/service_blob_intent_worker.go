package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) runBlobIntentReconciliation(ctx context.Context) {
	if err := s.reconcileBlobIntentTasks(ctx); err != nil && s.logger != nil {
		s.logger.Warn("blob intent reconciliation failed", "error_class", "storage")
	}
	anchoredReuseCursor := anchoredManifestReuseCursor{}
	if gate := s.gate.State(); gate.State == "open" && !gate.RecoveryBarrier {
		if next, _, err := s.reconcileAnchoredManifestReusePage(ctx, anchoredReuseCursor); err != nil {
			if s.logger != nil {
				s.logger.Warn("anchored manifest reuse reconciliation deferred", "error_class", "recovery")
			}
		} else {
			anchoredReuseCursor = next
		}
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	cacheScanActive := true
	cacheScanToken := ""
	for {
		select {
		case <-ctx.Done():
			return
		case identity := <-s.blobIntentEvents:
			eventTimeout := s.cfg.Limits.RequestTimeout
			if eventTimeout <= 0 {
				eventTimeout = 300 * time.Second
			}
			eventCtx, cancel := context.WithTimeout(ctx, eventTimeout)
			if err := s.processCachedBlobIntent(eventCtx, identity); err != nil {
				s.cacheIntentRescan.Store(true)
				if s.logger != nil {
					s.logger.Warn("cached blob intent activation deferred", "error_class", "storage")
				}
			}
			cancel()
		case <-ticker.C:
			gate := s.gate.State()
			if gate.RecoveryBarrier {
				recoveryCtx, cancel := context.WithTimeout(ctx, time.Minute)
				if err := s.recoverSolidificationProtection(recoveryCtx); err != nil && s.logger != nil {
					s.logger.Warn("solidification recovery remains blocked", "error_class", "recovery")
				}
				cancel()
			} else if err := s.reconcileTerminalTaskCleanup(ctx); err != nil && s.logger != nil {
				s.logger.Warn("terminal task cleanup remains pending", "error_class", "recovery")
			}
			if err := s.reconcileBlobIntentTasks(ctx); err != nil && s.logger != nil {
				s.logger.Warn("blob intent reconciliation failed", "error_class", "storage")
			}
			if s.cacheIntentRescan.Swap(false) {
				cacheScanActive = true
				cacheScanToken = ""
			}
			if cacheScanActive {
				next, err := s.reconcileCachedBlobIntentPage(ctx, cacheScanToken)
				if err != nil {
					if s.logger != nil {
						s.logger.Warn("cached blob intent reconciliation deferred", "error_class", "storage")
					}
				} else {
					cacheScanToken = next
					cacheScanActive = next != ""
				}
			}
			gate = s.gate.State()
			if gate.State == "open" && !gate.RecoveryBarrier {
				processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
				processor.wakeCoordinationWaiters(ctx)
				s.wakeAvailableQuotaWaiters(ctx, gate)
				if next, _, err := s.reconcileAnchoredManifestReusePage(ctx, anchoredReuseCursor); err != nil {
					if s.logger != nil {
						s.logger.Warn("anchored manifest reuse reconciliation deferred", "error_class", "recovery")
					}
				} else {
					anchoredReuseCursor = next
				}
			}
		}
	}
}

func (s *Service) wakeAvailableQuotaWaiters(ctx context.Context, gate gateRecord) {
	for _, reason := range []string{"distribution_quota", "protected_cache_capacity"} {
		if err := s.wakeAvailableQuotaWaiter(ctx, reason, gate); err != nil && s.logger != nil {
			s.logger.Warn("quota waiter reconciliation failed", "wait_reason", reason, "error_class", "storage")
		}
	}
}

func (s *Service) wakeAvailableQuotaWaiter(ctx context.Context, reason string, gate gateRecord) error {
	waiter, found, err := s.tasks.OldestWaitingQuota(ctx, reason)
	if err != nil || !found {
		return err
	}
	var fits bool
	switch reason {
	case "distribution_quota":
		maxSessions, maxBytes := protectionLimits(s.cfg)
		requested := waiter.QuotaWaitBytes
		if requested <= 0 {
			requested = waiter.RequestedBytes
		}
		if requested <= 0 {
			requested = waiter.BlobSize
		}
		replaceableBytes := min(waiter.QuotaReplaceableBytes, gate.ReservedDistributionBytes)
		usedBytes := gate.ReservedDistributionBytes - replaceableBytes
		replaceableSlots := min(waiter.QuotaReplaceableSlots, gate.NewProtectionSessionCount)
		usedSlots := gate.NewProtectionSessionCount - replaceableSlots
		fits = requested > 0 && usedSlots < maxSessions && usedBytes <= maxBytes && requested <= maxBytes-usedBytes
	case "protected_cache_capacity":
		limit := protectedCacheLimit(s.cfg)
		requested := waiter.BlobSize
		if requested <= 0 {
			requested = waiter.ProtectedCacheBytes
		}
		_, alreadyReserved := s.gate.ProtectedCacheReservation(waiter.ID)
		fits = alreadyReserved || requested > 0 && requested <= limit-gate.ProtectedCacheBytes
	default:
		return errors.New("quota wait reason is invalid")
	}
	if !fits {
		return nil
	}
	_, err = s.tasks.Wake(ctx, waiter.ID)
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) reconcileTerminalTaskCleanup(ctx context.Context) error {
	processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
	token := ""
	for {
		items, next, err := s.tasks.List(ctx, "", token, 500)
		if err != nil {
			return err
		}
		for _, task := range items {
			if !isTerminalTask(task.Status) || task.TerminalCleanupComplete {
				continue
			}
			if err := processor.TaskTerminal(ctx, &task); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}
