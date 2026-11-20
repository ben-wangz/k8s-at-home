package app

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

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
			if err := s.reconcileBlobIntent(ctx, intent); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (s *Service) reconcileBlobIntent(ctx context.Context, intent tasks.BlobIntent) error {
	if intent.ProtectionSessionID != "" && terminalBlobIntentState(intent.State) {
		settled, err := s.clearReleasedTerminalBlobIntentProtection(ctx, intent)
		if err != nil {
			return err
		}
		if settled {
			intent, err = s.records.GetBlobIntent(ctx, intent.ID)
			if err != nil {
				return err
			}
		}
	}
	if intent.State == tasks.IntentWaitingCache || intent.State == tasks.IntentWaitingQuota {
		cached, cacheErr := s.inspectReadyBlob(ctx, s.cache, intent.Identity.Authority, intent.Identity.Repository, intent.Identity.Digest)
		if cacheErr != nil {
			return cacheErr
		}
		if !cached.Present {
			if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(time.Now().UTC()) {
				if err := s.expireIdleBlobIntent(ctx, intent, time.Now().UTC()); err != nil {
					return err
				}
			}
			return nil
		}
		ready, readyErr := s.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
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
		return nil
	}
	if !intent.ExpiresAt.IsZero() && !intent.ExpiresAt.After(time.Now().UTC()) {
		if err := s.expireIdleBlobIntent(ctx, intent, time.Now().UTC()); err != nil {
			return err
		}
		return nil
	}
	if _, _, err := s.enqueueBlobIntent(ctx, intent); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
		if s.logger != nil {
			s.logger.Warn("blob intent reconciliation deferred", "intent_id", intent.ID, "error_class", "queue")
		}
		return err
	}
	return nil
}

func terminalBlobIntentState(state tasks.BlobIntentState) bool {
	return state == tasks.IntentAnchored || state == tasks.IntentFailed || state == tasks.IntentCancelled
}

func (s *Service) clearReleasedTerminalBlobIntentProtection(ctx context.Context, intent tasks.BlobIntent) (bool, error) {
	if !terminalBlobIntentState(intent.State) || intent.ProtectionSessionID == "" {
		return true, nil
	}
	if s.gate == nil {
		return false, errors.New("write gate is unavailable")
	}
	_, active, err := s.gate.protectionSession(ctx, intent.ProtectionSessionID)
	if err != nil || active {
		return false, err
	}
	if err := s.gate.confirmInactiveProtectionSessionReleased(ctx, intent.ProtectionSessionID, "", ""); err != nil {
		return false, err
	}
	if err := s.records.ClearBlobIntentProtectionSession(ctx, intent.ID, intent.ProtectionSessionID); err != nil {
		return false, err
	}
	current, err := s.records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		return false, err
	}
	return current.ProtectionSessionID == "", nil
}

func blobIntentCleanupWait(cause error) error {
	message := "previous blob intent cleanup is unresolved"
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	return fmt.Errorf("%w: %w: %s", tasks.ErrDemandAdmissionWaiting, storage.ErrConditionFailed, message)
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
