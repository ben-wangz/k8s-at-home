package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sync"
	"time"
)

func (s *Service) runBlobIntentReconciliation(ctx context.Context) {
	var group sync.WaitGroup
	for _, worker := range []func(context.Context){s.runIntentSourceReconciliation, s.runTaskReconciliation, s.runProtectionRecovery, s.runQuotaReconciliation} {
		group.Add(1)
		go func() { defer group.Done(); worker(ctx) }()
	}
	defer group.Wait()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case identity := <-s.blobIntentEvents:
			s.processCachedBlobEvent(ctx, identity)
		}
	}
}

func (s *Service) processCachedBlobEvent(ctx context.Context, identity tasks.BlobIdentity) {
	for ctx.Err() == nil {
		if s.recoveryBlocked() {
			s.observeQueueDependency("activation_events")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		bounded, release, ok := s.beginBackgroundUnit(ctx, "activation_events", 15*time.Second)
		if !ok {
			continue
		}
		err := s.processCachedBlobIntent(bounded, identity)
		release()
		s.observeQueueDependency("activation_events")
		if pureRecoveryPause(err) {
			continue
		}
		if err != nil {
			s.cacheIntentRescan.Store(true)
			s.backgroundEvent("activation", "deferred", err)
		}
		return
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
	return s.wakeFittingQuotaWaiter(ctx, reason, waiter, gate)
}

func (s *Service) wakeFittingQuotaWaiter(ctx context.Context, reason string, waiter tasks.Task, gate gateRecord) error {
	if gate.State != "open" || gate.RecoveryBarrier || waiter.CancelRequested {
		return nil
	}
	fits, err := s.quotaWaiterFits(ctx, reason, waiter, gate)
	if err != nil || !fits {
		return err
	}
	current := s.gate.State()
	if current.Epoch != gate.Epoch || current.State != "open" || current.RecoveryBarrier || s.recoveryStarting.Load() {
		return nil
	}
	_, err = s.tasks.WakeMatching(ctx, waiter)
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *Service) quotaWaiterFits(ctx context.Context, reason string, waiter tasks.Task, gate gateRecord) (bool, error) {
	switch reason {
	case "distribution_quota":
		covered, err := s.coveredDistributionBlob(ctx, waiter)
		if err != nil {
			return false, err
		}
		if covered {
			return true, nil
		}
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
		return requested > 0 && usedSlots < maxSessions && usedBytes <= maxBytes && requested <= maxBytes-usedBytes, nil
	case "protected_cache_capacity":
		limit := protectedCacheLimit(s.cfg)
		requested := waiter.BlobSize
		if requested <= 0 {
			requested = waiter.ProtectedCacheBytes
		}
		_, alreadyReserved := s.gate.ProtectedCacheReservation(waiter.ID)
		return alreadyReserved || requested > 0 && requested <= limit-gate.ProtectedCacheBytes, nil
	default:
		return false, errors.New("quota wait reason is invalid")
	}
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
