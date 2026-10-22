package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (r *Records) ExpireBlobIntent(ctx context.Context, id string, cutoff time.Time) (BlobIntent, bool, error) {
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, id)
		if err != nil {
			return BlobIntent{}, false, err
		}
		switch intent.State {
		case IntentWaitingCache, IntentWaitingQuota, IntentReady, IntentRetryWait:
		default:
			return intent, false, nil
		}
		if intent.ExpiresAt.IsZero() || intent.ExpiresAt.After(cutoff) {
			return intent, false, nil
		}
		intent.State = IntentCancelled
		intent.WakeAcknowledged = false
		intent.ErrorClass = "idle_expired"
		intent.ErrorMessage = "solidification intent expired without progress"
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := r.GetBlobIntent(ctx, id)
			if readErr != nil {
				return BlobIntent{}, false, errors.Join(err, readErr)
			}
			if latest.State == IntentCancelled && latest.ErrorClass == "idle_expired" {
				return latest, true, nil
			}
			continue
		} else if err != nil {
			return BlobIntent{}, false, err
		}
		return intent, true, nil
	}
	return BlobIntent{}, false, storage.ErrUnavailable
}

func (r *Records) SetBlobIntentProtectionSession(ctx context.Context, id, sessionID string) error {
	if sessionID != "" && !validID(sessionID) {
		return errors.New("blob protection session ID is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, id)
		if err != nil {
			return err
		}
		if intent.ProtectionSessionID == sessionID {
			return nil
		}
		intent.ProtectionSessionID = sessionID
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (r *Records) ClearBlobIntentProtectionSession(ctx context.Context, id, expectedSessionID string) error {
	if !validID(expectedSessionID) {
		return errors.New("blob protection session ID is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, id)
		if err != nil {
			return err
		}
		if intent.ProtectionSessionID != expectedSessionID {
			return nil
		}
		intent.ProtectionSessionID = ""
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (r *Records) ListBlobIntents(ctx context.Context, token string, limit int) ([]BlobIntent, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := r.store.List(ctx, "solidification/blobs/intents/", token, limit)
	if err != nil {
		return nil, "", err
	}
	intents := make([]BlobIntent, 0, len(entries))
	for _, entry := range entries {
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "solidification/blobs/intents/"), ".json")
		intent, getErr := r.GetBlobIntent(ctx, id)
		if getErr != nil {
			return nil, "", getErr
		}
		intents = append(intents, intent)
	}
	return intents, next, nil
}

func (r *Records) RebuildRootBlobDemandIndexes(ctx context.Context) error {
	intentToken := ""
	for {
		intents, nextIntent, err := r.ListBlobIntents(ctx, intentToken, 500)
		if err != nil {
			return err
		}
		for _, intent := range intents {
			demandToken := ""
			for {
				demands, nextDemand, err := r.ListBlobDemandRecords(ctx, intent.ID, demandToken, 500)
				if err != nil {
					return err
				}
				for _, demand := range demands {
					if err := r.saveRootBlobDemandIndex(ctx, demand); err != nil {
						return err
					}
				}
				if nextDemand == "" {
					break
				}
				demandToken = nextDemand
			}
		}
		if nextIntent == "" {
			return nil
		}
		intentToken = nextIntent
	}
}

func (r *Records) checkBlobIntentCapacity(ctx context.Context) error {
	return r.checkBlobIntentCapacityExcept(ctx, "")
}

func (r *Records) checkBlobIntentCapacityExcept(ctx context.Context, exceptID string) error {
	limit := r.maxBlobIntents
	if limit < 1 {
		limit = 10000
	}
	active := 0
	token := ""
	for {
		items, next, err := r.ListBlobIntents(ctx, token, 1000)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return nil
			}
			return err
		}
		for _, item := range items {
			if item.ID == exceptID {
				continue
			}
			if blobIntentConsumesCapacity(item.State) {
				active++
				if active >= limit {
					return errors.New("solidification blob intent capacity is exhausted")
				}
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}
