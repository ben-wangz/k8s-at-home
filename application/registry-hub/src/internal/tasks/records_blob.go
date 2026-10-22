package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"time"
)

func (r *Records) EnsureBlobIntent(ctx context.Context, identity BlobIdentity, generation uint64, demandID string) (BlobIntent, bool, error) {
	if r == nil || r.store == nil {
		return BlobIntent{}, false, errors.New("solidification record store is unavailable")
	}
	r.capacityMu.Lock()
	defer r.capacityMu.Unlock()
	if err := validateBlobIdentity(identity); err != nil {
		return BlobIntent{}, false, err
	}
	if demandID != "" && !validID(demandID) {
		return BlobIntent{}, false, errors.New("blob intent demand ID is invalid")
	}
	identityHash, err := recordHash(identity)
	if err != nil {
		return BlobIntent{}, false, err
	}
	indexKey := "solidification/blobs/index/" + identityHash + ".json"
	lastCandidateID := ""
	for attempt := 0; attempt < 32; attempt++ {
		index, indexEntry, readErr := readBlobIntentIndex(ctx, r.store, indexKey)
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return BlobIntent{}, false, readErr
		}
		if readErr == nil {
			current, getErr := r.GetBlobIntent(ctx, index.IntentID)
			if getErr != nil {
				return BlobIntent{}, false, fmt.Errorf("read current blob intent: %w", getErr)
			}
			if current.Identity != identity {
				return BlobIntent{}, false, errors.New("blob intent index identity mismatch")
			}
			if blobIntentActive(current.State) && current.Generation == generation {
				if demandID != "" {
					if err := r.LinkBlobDemand(ctx, current.ID, demandID); err != nil {
						return BlobIntent{}, false, err
					}
				}
				latestIndex, err := r.store.Head(ctx, indexKey)
				if err != nil {
					return BlobIntent{}, false, err
				}
				if latestIndex.Version != indexEntry.Version {
					continue
				}
				return current, current.ID == lastCandidateID, nil
			}
			if err := r.checkBlobIntentCapacity(ctx); err != nil {
				return BlobIntent{}, false, err
			}
			intent, createErr := r.createBlobIntent(ctx, identity, current.Round+1, generation, demandID)
			if createErr != nil {
				return BlobIntent{}, false, createErr
			}
			lastCandidateID = intent.ID
			if err := r.writeBlobIntentIndex(ctx, indexKey, intent.ID, indexEntry.Version); errors.Is(err, storage.ErrConditionFailed) {
				r.discardBlobIntent(intent, demandID)
				lastCandidateID = ""
				continue
			} else if errors.Is(err, storage.ErrUnknownCommit) {
				continue
			} else if err != nil {
				r.discardBlobIntent(intent, demandID)
				return BlobIntent{}, false, err
			}
			return intent, true, nil
		}

		if err := r.checkBlobIntentCapacity(ctx); err != nil {
			return BlobIntent{}, false, err
		}
		intent, createErr := r.createBlobIntent(ctx, identity, 1, generation, demandID)
		if createErr != nil {
			return BlobIntent{}, false, createErr
		}
		lastCandidateID = intent.ID
		if err := r.writeBlobIntentIndex(ctx, indexKey, intent.ID, ""); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			r.discardBlobIntent(intent, demandID)
			lastCandidateID = ""
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else if err != nil {
			r.discardBlobIntent(intent, demandID)
			return BlobIntent{}, false, err
		}
		return intent, true, nil
	}
	return BlobIntent{}, false, storage.ErrUnavailable
}

func (r *Records) SetBlobIntentReady(ctx context.Context, id, mediaType, dataVersion string, actualSize int64) (BlobIntent, error) {
	if mediaType == "" || dataVersion == "" || actualSize < 0 {
		return BlobIntent{}, errors.New("verified cache identity is invalid")
	}
	intentID := id
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, intentID)
		if err != nil {
			return BlobIntent{}, err
		}
		current, err := r.GetBlobIntentByIdentity(ctx, intent.Identity)
		if err != nil {
			return BlobIntent{}, err
		}
		if current.ID != intent.ID {
			intentID = current.ID
			continue
		}
		if intent.State == IntentAnchored || intent.State == IntentReady || intent.State == IntentRunning || intent.State == IntentRetryWait {
			if intent.MediaType != mediaType || intent.CacheVersion != dataVersion || intent.ActualSize != actualSize {
				return BlobIntent{}, errors.New("blob intent cache version changed within an active round")
			}
			return intent, nil
		}
		if intent.State == IntentVerified {
			if intent.MediaType == mediaType && intent.CacheVersion == dataVersion && intent.ActualSize == actualSize {
				return intent, nil
			}
			next, err := r.StartBlobIntentRound(ctx, intent.ID)
			if err != nil {
				return BlobIntent{}, err
			}
			intentID = next.ID
			continue
		}
		if intent.State != IntentWaitingCache && intent.State != IntentWaitingQuota {
			if intent.State != IntentVerified {
				return BlobIntent{}, errors.New("blob intent is not waiting for a cache object")
			}
		}
		previousState := intent.State
		intent.CacheVersion = dataVersion
		intent.ActualSize = actualSize
		intent.MediaType = mediaType
		intent.State = IntentReady
		intent.WakeAcknowledged = false
		if previousState != IntentReady {
			intent.LastProgressAt = time.Now().UTC()
			intent.ExpiresAt = r.idleExpiry(intent.LastProgressAt)
		}
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := r.GetBlobIntent(ctx, intentID)
			if readErr == nil && latest.State == IntentReady && latest.MediaType == mediaType && latest.CacheVersion == dataVersion && latest.ActualSize == actualSize {
				current, indexErr := r.GetBlobIntentByIdentity(ctx, latest.Identity)
				if indexErr != nil {
					return BlobIntent{}, indexErr
				}
				if current.ID == latest.ID {
					return latest, nil
				}
				intentID = current.ID
			}
			continue
		} else if err != nil {
			return BlobIntent{}, err
		}
		current, err = r.GetBlobIntentByIdentity(ctx, intent.Identity)
		if err != nil {
			return BlobIntent{}, err
		}
		if current.ID != intent.ID {
			intentID = current.ID
			continue
		}
		return intent, nil
	}
	return BlobIntent{}, storage.ErrUnavailable
}
