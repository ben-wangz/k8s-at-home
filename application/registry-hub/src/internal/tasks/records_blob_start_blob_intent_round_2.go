package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (r *Records) StartBlobIntentRound(ctx context.Context, id string) (BlobIntent, error) {
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	current, err := r.GetBlobIntent(ctx, id)
	if err != nil {
		return BlobIntent{}, err
	}
	identityHash, err := recordHash(current.Identity)
	if err != nil {
		return BlobIntent{}, err
	}
	indexKey := "solidification/blobs/index/" + identityHash + ".json"
	for attempt := 0; attempt < 16; attempt++ {
		index, indexEntry, err := readBlobIntentIndex(ctx, r.store, indexKey)
		if err != nil {
			return BlobIntent{}, err
		}
		latest, err := r.GetBlobIntent(ctx, index.IntentID)
		if err != nil {
			return BlobIntent{}, err
		}
		if latest.ID != current.ID {
			if blobIntentActive(latest.State) {
				return latest, nil
			}
			current = latest
		}
		if blobIntentActive(current.State) && current.State != IntentVerified {
			return current, nil
		}
		if err := r.checkBlobIntentCapacityExcept(ctx, current.ID); err != nil {
			return BlobIntent{}, err
		}
		next, err := r.createBlobIntent(ctx, current.Identity, current.Round+1, current.Generation, "")
		if err != nil {
			return BlobIntent{}, err
		}
		next.ProtectionSessionID = current.ProtectionSessionID
		if err := r.SaveBlobIntent(ctx, &next); err != nil {
			r.discardBlobIntentRound(next)
			return BlobIntent{}, err
		}
		if err := r.copyBlobDemands(ctx, current.ID, next.ID); err != nil {
			r.discardBlobIntentRound(next)
			return BlobIntent{}, err
		}
		if err := r.writeBlobIntentIndex(ctx, indexKey, next.ID, indexEntry.Version); err == nil {
			return next, nil
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latestIndex, _, readErr := readBlobIntentIndex(ctx, r.store, indexKey)
			if readErr != nil {
				r.discardBlobIntentRound(next)
				return BlobIntent{}, errors.Join(err, readErr)
			}
			if latestIndex.IntentID == next.ID {
				return next, nil
			}
			r.discardBlobIntentRound(next)
			continue
		} else if errors.Is(err, storage.ErrConditionFailed) {
			r.discardBlobIntentRound(next)
			continue
		} else {
			r.discardBlobIntentRound(next)
			return BlobIntent{}, err
		}
	}
	return BlobIntent{}, storage.ErrUnavailable
}

func (r *Records) copyBlobDemands(ctx context.Context, sourceID, targetID string) error {
	token := ""
	for {
		items, next, err := r.ListBlobDemandRecords(ctx, sourceID, token, 500)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, item := range items {
			if item.RootTaskID == "" {
				if err := r.LinkBlobDemand(ctx, targetID, targetID); err != nil {
					return err
				}
				continue
			}
			if err := r.LinkBlobDemandScope(ctx, targetID, item.DemandID, item.RootTaskID, item.ScopeID, item.SessionID, item.ReservedBytes); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (r *Records) discardBlobIntentRound(intent BlobIntent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	identityHash, err := recordHash(intent.Identity)
	if err != nil {
		return
	}
	index, _, err := readBlobIntentIndex(ctx, r.store, "solidification/blobs/index/"+identityHash+".json")
	if err != nil || index.IntentID == intent.ID {
		return
	}
	current, err := r.GetBlobIntent(ctx, intent.ID)
	if err != nil || !sameBlobIntentIdentity(current, intent.Identity, intent.Round, intent.Generation) || current.State != IntentWaitingCache && current.State != IntentCancelled {
		return
	}
	if current.State != IntentCancelled {
		current, err = r.SetBlobIntentState(ctx, intent.ID, IntentCancelled, "orphaned_round", "unpublished blob intent round")
		if err != nil {
			return
		}
	}
	prefix := "solidification/blobs/owners/" + intent.ID + "/"
	token := ""
	for {
		entries, next, err := r.store.List(ctx, prefix, token, 500)
		if err != nil {
			break
		}
		for _, entry := range entries {
			_ = r.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
		}
		if next == "" {
			break
		}
		token = next
	}
	if current.version != "" {
		_ = r.store.DeleteIfMatch(ctx, blobIntentKey(intent.ID), current.version)
	}
}

func (r *Records) SetBlobIntentState(ctx context.Context, id string, state BlobIntentState, class, message string) (BlobIntent, error) {
	if !validBlobIntentState(state) {
		return BlobIntent{}, errors.New("blob intent state is invalid")
	}
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, id)
		if err != nil {
			return BlobIntent{}, err
		}
		if intent.State == IntentAnchored && state != IntentAnchored {
			return intent, nil
		}
		if intent.State == state && intent.ErrorClass == class && intent.ErrorMessage == message {
			return intent, nil
		}
		previousState := intent.State
		intent.State = state
		intent.WakeAcknowledged = false
		intent.ErrorClass = class
		intent.ErrorMessage = message
		if state == IntentVerified && previousState != IntentVerified || state == IntentAnchored && previousState != IntentAnchored {
			intent.LastProgressAt = time.Now().UTC()
			intent.ExpiresAt = r.idleExpiry(intent.LastProgressAt)
		}
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := r.GetBlobIntent(ctx, id)
			if readErr == nil && latest.State == state && latest.ErrorClass == class && latest.ErrorMessage == message {
				return latest, nil
			}
			continue
		} else if err != nil {
			return BlobIntent{}, err
		}
		return intent, nil
	}
	return BlobIntent{}, storage.ErrUnavailable
}
