package app

import (
	"context"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) ensureBlobIntent(ctx context.Context, routed route.Route) (tasks.BlobIntent, error) {
	return s.ensureBlobIntentForIdentity(ctx, tasks.BlobIdentity{
		Authority: routed.Upstream, Repository: routed.RemoteRepo, Digest: routed.Reference,
	})
}

func (s *Service) ensureBlobIntentForIdentity(ctx context.Context, identity tasks.BlobIdentity) (tasks.BlobIntent, error) {
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, identity.Digest)
	if err != nil {
		return tasks.BlobIntent{}, err
	}
	intent, _, err := s.records.EnsureBlobIntent(ctx, identity, generation.Generation, "")
	if err != nil {
		return tasks.BlobIntent{}, err
	}
	if err := s.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		return tasks.BlobIntent{}, err
	}
	return intent, nil
}

func (s *Service) activateBlobIntent(ctx context.Context, intent tasks.BlobIntent, cached artifact.CachedObject) error {
	ready, err := s.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		return err
	}
	if ready.ID != intent.ID {
		if err := s.records.LinkBlobDemand(ctx, ready.ID, ready.ID); err != nil {
			return err
		}
	}
	if ready.State == tasks.IntentAnchored || ready.State == tasks.IntentVerified {
		return nil
	}
	if ready.State != tasks.IntentReady && ready.State != tasks.IntentRetryWait {
		return nil
	}
	blobTask, _, enqueueErr := s.enqueueBlobIntent(ctx, ready)
	if enqueueErr != nil && s.logger != nil {
		s.logger.Warn("blob intent is ready but task admission is deferred", "intent_id", ready.ID, "error_class", "queue")
	} else {
		ready.ID = blobTask.ID
		if blobTask.Status == tasks.VerifiedLocal || blobTask.Status == tasks.WaitingDeps && blobTask.WaitReason == "manifest_scope_admission" {
			_, _ = s.tasks.Wake(ctx, blobTask.ID)
		}
	}
	s.wakeBlobDemandRoots(ctx, ready.ID)
	return nil
}

func (s *Service) enqueueBlobIntent(ctx context.Context, intent tasks.BlobIntent) (tasks.Task, bool, error) {
	current, err := s.records.GetBlobIntentByIdentity(ctx, intent.Identity)
	if err != nil {
		return tasks.Task{}, false, err
	}
	if current.State != tasks.IntentReady && current.State != tasks.IntentRetryWait {
		return tasks.Task{}, false, storage.ErrConditionFailed
	}
	intent = current
	var session protectionSessionRecord
	protected := false
	if intent.ProtectionSessionID != "" {
		if s.gate.State().RecoveryBarrier {
			return tasks.Task{}, false, ErrRecoveryBarrier
		}
		var active bool
		var err error
		session, active, err = s.gate.protectionSession(ctx, intent.ProtectionSessionID)
		if err != nil {
			return tasks.Task{}, false, err
		}
		if !active {
			persisted, readErr := s.gate.readProtectionSession(ctx, intent.ProtectionSessionID)
			if errors.Is(readErr, storage.ErrNotFound) {
				barrierErr := s.gate.SetRecoveryBarrier(ctx, "blob_owner_unknown")
				return tasks.Task{}, false, errors.Join(ErrRecoveryBarrier, barrierErr)
			}
			if readErr != nil {
				return tasks.Task{}, false, readErr
			}
			switch persisted.State {
			case "transferred":
				return tasks.Task{}, false, ErrRecoveryBarrier
			case "released":
				if persisted.TerminalReason == "anchored" {
					return tasks.Task{}, false, ErrProtectionAnchored
				}
				return tasks.Task{}, false, storage.ErrConditionFailed
			default:
				barrierErr := s.gate.SetRecoveryBarrier(ctx, "blob_owner_unknown")
				return tasks.Task{}, false, errors.Join(ErrRecoveryBarrier, barrierErr)
			}
		}
		if session.State != "protected" && session.State != "anchoring" {
			return tasks.Task{}, false, ErrProtectionAnchored
		}
		if err := s.gate.AddProtectionOwner(ctx, session.ID, intent.ID); err != nil {
			return tasks.Task{}, false, err
		}
		protected = true
	}
	var task tasks.Task
	var created bool
	if protected {
		task, created, err = s.tasks.EnqueueBlobProtected(ctx, intent, session.ScopeID, session.ReservedBytes)
	} else {
		task, created, err = s.tasks.EnqueueBlob(ctx, intent)
	}
	if err != nil {
		return tasks.Task{}, false, err
	}
	if protected {
		if err := s.tasks.ReconcileProtectionState(ctx, task.ID, session.ScopeID, []string{session.ID}, session.ReservedBytes, session.ReservedBytes); err != nil {
			return task, created, err
		}
	}
	return task, created, nil
}

func (s *Service) scheduleCachedBlobIntent(identity tasks.BlobIdentity) {
	select {
	case s.blobIntentEvents <- identity:
	default:
		s.cacheIntentRescan.Store(true)
	}
}

func (s *Service) processCachedBlobIntent(ctx context.Context, identity tasks.BlobIdentity) error {
	cached, err := s.cache.GetAnyRetained(ctx, identity.Authority, identity.Repository, identity.Digest)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer cached.Body.Close()
	intent, err := s.ensureBlobIntentForIdentity(ctx, identity)
	if err != nil {
		return err
	}
	return s.activateBlobIntent(ctx, intent, cached)
}

func (s *Service) reconcileCachedBlobIntentPage(ctx context.Context, token string) (string, error) {
	items, next, err := s.cache.ListCompleteRepresentations(ctx, token, 500)
	if err != nil {
		return token, err
	}
	seen := make(map[tasks.BlobIdentity]struct{}, len(items))
	for _, item := range items {
		if isManifestRepresentation(item.MediaType) {
			continue
		}
		identity := tasks.BlobIdentity{Authority: item.Authority, Repository: item.Repository, Digest: item.Digest}
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		_, intentErr := s.records.GetBlobIntentByIdentity(ctx, identity)
		if intentErr == nil {
			continue
		}
		if !errors.Is(intentErr, storage.ErrNotFound) {
			return token, intentErr
		}
		select {
		case s.blobIntentEvents <- identity:
		default:
			return token, nil
		}
	}
	return next, nil
}

func isManifestRepresentation(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json":
		return true
	default:
		return false
	}
}

func (s *Service) reconcileBlobIntentTasks(ctx context.Context) error {
	token := ""
	for {
		intents, next, err := s.records.ListBlobIntents(ctx, token, 500)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			return err
		}
		for _, intent := range intents {
			if intent.ProtectionSessionID != "" && terminalBlobIntentState(intent.State) {
				_, active, sessionErr := s.gate.protectionSession(ctx, intent.ProtectionSessionID)
				if sessionErr != nil {
					return sessionErr
				}
				if !active {
					if err := s.records.ClearBlobIntentProtectionSession(ctx, intent.ID, intent.ProtectionSessionID); err != nil {
						return err
					}
					intent.ProtectionSessionID = ""
				}
			}
			if intent.State == tasks.IntentWaitingCache || intent.State == tasks.IntentWaitingQuota {
				cached, cacheErr := s.cache.GetAnyRetained(ctx, intent.Identity.Authority, intent.Identity.Repository, intent.Identity.Digest)
				if errors.Is(cacheErr, storage.ErrNotFound) {
					if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(time.Now().UTC()) {
						if err := s.expireIdleBlobIntent(ctx, intent, time.Now().UTC()); err != nil {
							return err
						}
					}
					continue
				}
				if cacheErr != nil {
					return cacheErr
				}
				ready, readyErr := s.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
				_ = cached.Body.Close()
				if readyErr != nil {
					return readyErr
				}
				intent = ready
			}
			if !intent.WakeAcknowledged && (intent.State == tasks.IntentReady || intent.State == tasks.IntentVerified || terminalBlobIntentState(intent.State)) {
				if err := s.notifyBlobDemandRoots(ctx, intent.ID); err != nil {
					return err
				}
			}
			if intent.State != tasks.IntentReady && intent.State != tasks.IntentRetryWait {
				continue
			}
			if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(time.Now().UTC()) {
				if err := s.expireIdleBlobIntent(ctx, intent, time.Now().UTC()); err != nil {
					return err
				}
				continue
			}
			if _, _, err := s.enqueueBlobIntent(ctx, intent); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
				if s.logger != nil {
					s.logger.Warn("blob intent reconciliation deferred", "intent_id", intent.ID, "error_class", "queue")
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func terminalBlobIntentState(state tasks.BlobIntentState) bool {
	return state == tasks.IntentAnchored || state == tasks.IntentFailed || state == tasks.IntentCancelled
}

func (s *Service) expireIdleBlobIntent(ctx context.Context, intent tasks.BlobIntent, now time.Time) error {
	switch intent.State {
	case tasks.IntentWaitingCache, tasks.IntentWaitingQuota, tasks.IntentReady, tasks.IntentRetryWait:
	default:
		return nil
	}
	if intent.ExpiresAt.IsZero() || intent.ExpiresAt.After(now) {
		return nil
	}
	task, err := s.tasks.Get(ctx, intent.ID)
	if err == nil {
		if task.Status == tasks.Cancelled || task.Status == tasks.Failed || task.Status == tasks.Solidified {
			return nil
		}
		if task.CancelRequested || task.ExpiresAt.IsZero() || task.ExpiresAt.After(now) {
			return nil
		}
		_, err := s.tasks.CancelIfExpired(ctx, task.ID, now)
		return err
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if intent.ProtectionSessionID != "" {
		_, active, sessionErr := s.gate.protectionSession(ctx, intent.ProtectionSessionID)
		if sessionErr != nil {
			return sessionErr
		}
		if active {
			return nil
		}
		if err := s.records.ClearBlobIntentProtectionSession(ctx, intent.ID, intent.ProtectionSessionID); err != nil {
			return err
		}
	}
	_, expired, err := s.records.ExpireBlobIntent(ctx, intent.ID, now)
	if err != nil {
		return err
	}
	if expired {
		s.wakeBlobDemandRoots(ctx, intent.ID)
	}
	return nil
}

func (s *Service) runBlobIntentReconciliation(ctx context.Context) {
	if err := s.reconcileBlobIntentTasks(ctx); err != nil && s.logger != nil {
		s.logger.Warn("blob intent reconciliation failed", "error_class", "storage")
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
