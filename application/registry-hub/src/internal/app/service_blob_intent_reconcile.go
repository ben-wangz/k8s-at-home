package app

import (
	"context"
	"errors"
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
