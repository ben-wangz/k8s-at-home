package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) cancelBlobIntentWithoutDemand(ctx context.Context, intentID string) (string, error) {
	intent, err := s.service.records.GetBlobIntent(ctx, intentID)
	if errors.Is(err, storage.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "demand_intent_read_unknown", err
	}
	active, err := s.hasActiveBlobDemand(ctx, intentID)
	if err != nil {
		return "demand_active_check_unknown", err
	}
	if active {
		s.service.wakeBlobDemandRoots(ctx, intentID)
		return "", nil
	}
	task, err := s.tasks.Get(ctx, intentID)
	if errors.Is(err, storage.ErrNotFound) {
		if intent.ProtectionSessionID != "" {
			remaining, removeErr := s.gate.RemoveProtectionOwner(ctx, intent.ProtectionSessionID, intentID)
			if removeErr != nil && !errors.Is(removeErr, storage.ErrNotFound) {
				return "demand_owner_remove_unknown", removeErr
			}
			if removeErr == nil && remaining == 0 {
				if err := s.releaseUnownedProtectionSession(ctx, intent.ProtectionSessionID); err != nil {
					return "demand_protection_release_unknown", err
				}
			}
		}
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentCancelled, "no_demand", "no active blob demands remain")
		}
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return "", nil
			}
			return "demand_intent_cancel_unknown", err
		}
		return "", nil
	}
	if err != nil {
		return "demand_task_read_unknown", err
	}
	switch task.Status {
	case tasks.RepairPending:
		return "", nil
	case tasks.Solidified:
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentAnchored, "", "")
		}
		if err != nil {
			return "demand_intent_anchor_unknown", err
		}
		return "", nil
	case tasks.Failed, tasks.Cancelled:
		if blobIntentNeedsSolidification(intent.State) {
			_, err = s.service.records.SetBlobIntentState(ctx, intentID, tasks.IntentCancelled, "no_demand", "no active blob demands remain")
		}
		if err != nil {
			return "demand_intent_cancel_unknown", err
		}
		return "", nil
	default:
		_, err := s.tasks.Cancel(ctx, intentID)
		if err != nil {
			return "demand_task_cancel_unknown", err
		}
		return "", nil
	}
}

func (s *solidifier) releaseUnownedProtectionSession(ctx context.Context, sessionID string) error {
	session, active, err := s.gate.protectionSession(ctx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult {
		return ErrProtectionBusy
	}
	stop, err := s.gate.RequestProtectionStop(ctx, sessionID, "cancelled")
	if err != nil || !stop {
		return err
	}
	session, active, err = s.gate.protectionSession(ctx, sessionID)
	if err != nil || !active {
		return err
	}
	if session.State == "anchored" {
		return s.gate.ReleaseProtection(ctx, sessionID, "anchored")
	}
	if session.State != "quiesced" {
		if session.State != "quiescing" {
			if err := s.gate.MarkProtectionQuiescing(ctx, sessionID); err != nil {
				return err
			}
		}
		session, active, err = s.gate.protectionSession(ctx, sessionID)
		if err != nil || !active {
			return err
		}
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return ErrProtectionBusy
		}
		if err := s.gate.MarkProtectionQuiesced(ctx, sessionID); err != nil {
			return err
		}
	}
	return s.gate.ReleaseProtection(ctx, sessionID, "cancelled")
}

func (s *solidifier) hasActiveBlobDemand(ctx context.Context, intentID string) (bool, error) {
	token := ""
	for {
		demands, next, err := s.service.records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, demand := range demands {
			if demand.RootTaskID == "" {
				return true, nil
			}
			root, err := s.tasks.Get(ctx, demand.RootTaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if !root.CancelRequested && !isTerminalTask(root.Status) {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func blobIntentNeedsSolidification(state tasks.BlobIntentState) bool {
	switch state {
	case tasks.IntentWaitingCache, tasks.IntentWaitingQuota, tasks.IntentReady, tasks.IntentRunning, tasks.IntentRetryWait, tasks.IntentVerified:
		return true
	default:
		return false
	}
}
