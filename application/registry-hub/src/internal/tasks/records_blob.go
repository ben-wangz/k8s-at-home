package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/storage"
	"strings"
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

func (r *Records) createBlobIntent(ctx context.Context, identity BlobIdentity, round, generation uint64, demandID string) (BlobIntent, error) {
	id, err := randomID()
	if err != nil {
		return BlobIntent{}, err
	}
	now := time.Now().UTC()
	intent := BlobIntent{
		SchemaVersion: solidificationRecordSchema, ID: id, Identity: identity, Round: round, Generation: generation,
		State: IntentWaitingCache, CreatedAt: now, UpdatedAt: now, LastProgressAt: now, ExpiresAt: r.idleExpiry(now),
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return BlobIntent{}, err
	}
	entry, err := r.store.Put(ctx, blobIntentKey(id), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil {
		if errors.Is(err, storage.ErrUnknownCommit) {
			loaded, loadErr := r.GetBlobIntent(ctx, id)
			if loadErr == nil && sameBlobIntentIdentity(loaded, identity, round, generation) {
				if demandID != "" {
					if ownerErr := r.LinkBlobDemand(ctx, id, demandID); ownerErr != nil {
						return BlobIntent{}, ownerErr
					}
				}
				return loaded, nil
			}
			return BlobIntent{}, errors.Join(err, loadErr)
		}
		return BlobIntent{}, err
	}
	intent.version = entry.Version
	if demandID != "" {
		if err := r.LinkBlobDemand(ctx, id, demandID); err != nil {
			intent.version = entry.Version
			r.discardBlobIntent(intent, demandID)
			return BlobIntent{}, err
		}
	}
	return intent, nil
}

func (r *Records) GetBlobIntent(ctx context.Context, id string) (BlobIntent, error) {
	if !validID(id) {
		return BlobIntent{}, storage.ErrNotFound
	}
	object, err := r.store.Get(ctx, blobIntentKey(id))
	if err != nil {
		return BlobIntent{}, err
	}
	var intent BlobIntent
	if err := decodeRecord(object, 1<<20, &intent); err != nil {
		return BlobIntent{}, err
	}
	if intent.SchemaVersion != solidificationRecordSchema || intent.ID != id || !validBlobIntentState(intent.State) || validateBlobIdentity(intent.Identity) != nil || intent.Round == 0 {
		return BlobIntent{}, errors.New("blob intent record is invalid")
	}
	if intent.ExpiresAt.IsZero() {
		progressAt := intent.LastProgressAt
		if progressAt.IsZero() {
			progressAt = intent.UpdatedAt
		}
		if progressAt.IsZero() {
			progressAt = intent.CreatedAt
		}
		if !progressAt.IsZero() {
			intent.ExpiresAt = r.idleExpiry(progressAt)
		}
	}
	intent.version = object.Version
	return intent, nil
}

func (r *Records) GetBlobIntentByIdentity(ctx context.Context, identity BlobIdentity) (BlobIntent, error) {
	if err := validateBlobIdentity(identity); err != nil {
		return BlobIntent{}, err
	}
	identityHash, err := recordHash(identity)
	if err != nil {
		return BlobIntent{}, err
	}
	index, _, err := readBlobIntentIndex(ctx, r.store, "solidification/blobs/index/"+identityHash+".json")
	if err != nil {
		return BlobIntent{}, err
	}
	intent, err := r.GetBlobIntent(ctx, index.IntentID)
	if err != nil {
		return BlobIntent{}, err
	}
	if intent.Identity != identity {
		return BlobIntent{}, errors.New("blob intent index identity mismatch")
	}
	return intent, nil
}

func (r *Records) SaveBlobIntent(ctx context.Context, intent *BlobIntent) error {
	if intent == nil || !validID(intent.ID) || intent.SchemaVersion != solidificationRecordSchema || !validBlobIntentState(intent.State) || validateBlobIdentity(intent.Identity) != nil || intent.Round == 0 || intent.version == "" {
		return errors.New("blob intent state is invalid or has no storage version")
	}
	intent.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	entry, err := r.store.Put(ctx, blobIntentKey(intent.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: intent.version})
	if err != nil {
		return err
	}
	intent.version = entry.Version
	return nil
}

func (r *Records) LinkBlobDemand(ctx context.Context, intentID, demandID string) error {
	return r.LinkBlobDemandScope(ctx, intentID, demandID, "", "", "", 0)
}

func (r *Records) LinkBlobDemandScope(ctx context.Context, intentID, demandID, rootTaskID, scopeID, sessionID string, reservedBytes int64) error {
	if !validID(intentID) || !validID(demandID) {
		return errors.New("blob intent and demand IDs must be valid")
	}
	if rootTaskID != "" && !validID(rootTaskID) {
		return errors.New("blob root task ID is invalid")
	}
	if reservedBytes < 0 || scopeID == "" && (sessionID != "" || reservedBytes != 0) || scopeID != "" && (sessionID == "" || !validID(sessionID)) {
		return errors.New("blob demand protection scope is invalid")
	}
	if _, err := r.GetBlobIntent(ctx, intentID); err != nil {
		return err
	}
	key := blobDemandKey(intentID, demandID)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, readErr := r.readBlobDemand(ctx, key)
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return readErr
		}
		if errors.Is(readErr, storage.ErrNotFound) {
			current = BlobDemand{SchemaVersion: solidificationRecordSchema, IntentID: intentID, DemandID: demandID, CreatedAt: time.Now().UTC()}
		}
		if current.ScopeID != "" && scopeID != "" && (current.RootTaskID != rootTaskID || current.ScopeID != scopeID || current.SessionID != sessionID || current.ReservedBytes != reservedBytes) {
			return storage.ErrConditionFailed
		}
		if scopeID != "" {
			current.RootTaskID = rootTaskID
			current.ScopeID = scopeID
			current.SessionID = sessionID
			current.ReservedBytes = reservedBytes
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: version == ""}
		if version != "" {
			options = storage.PutOptions{IfMatch: version}
		}
		_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrUnknownCommit) {
			if errors.Is(err, storage.ErrUnknownCommit) {
				loaded, _, verifyErr := r.readBlobDemand(ctx, key)
				if verifyErr == nil && loaded.IntentID == intentID && loaded.DemandID == demandID && (scopeID == "" || loaded.RootTaskID == rootTaskID && loaded.ScopeID == scopeID && loaded.SessionID == sessionID && loaded.ReservedBytes == reservedBytes) {
					if err := r.saveRootBlobDemandIndex(ctx, loaded); err != nil {
						return err
					}
					return r.markBlobIntentDemandProgress(ctx, intentID, loaded.CreatedAt)
				}
			}
			continue
		}
		if err != nil {
			return err
		}
		if err := r.saveRootBlobDemandIndex(ctx, current); err != nil {
			return err
		}
		return r.markBlobIntentDemandProgress(ctx, intentID, current.CreatedAt)
	}
	return storage.ErrUnavailable
}

func (r *Records) saveRootBlobDemandIndex(ctx context.Context, link BlobDemand) error {
	if link.RootTaskID == "" {
		return nil
	}
	key := rootBlobDemandIndexKey(link.RootTaskID, link.IntentID, link.DemandID)
	data, err := json.Marshal(link)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		object, getErr := r.store.Get(ctx, key)
		version := ""
		if getErr == nil {
			var existing BlobDemand
			if err := decodeRecord(object, 1<<20, &existing); err != nil {
				return err
			}
			version = object.Version
			if existing.IntentID != link.IntentID || existing.DemandID != link.DemandID || existing.RootTaskID != link.RootTaskID {
				return errors.New("root blob demand index conflicts with its identity")
			}
			currentData, err := json.Marshal(existing)
			if err != nil {
				return err
			}
			if bytes.Equal(currentData, data) {
				return nil
			}
		} else if !errors.Is(getErr, storage.ErrNotFound) {
			return getErr
		}
		options := storage.PutOptions{IfAbsent: version == ""}
		if version != "" {
			options = storage.PutOptions{IfMatch: version}
		}
		_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}

func (r *Records) markBlobIntentDemandProgress(ctx context.Context, intentID string, progressAt time.Time) error {
	for attempt := 0; attempt < 16; attempt++ {
		intent, err := r.GetBlobIntent(ctx, intentID)
		if err != nil {
			return err
		}
		if !intent.LastProgressAt.Before(progressAt) {
			return nil
		}
		intent.LastProgressAt = progressAt
		deadline := r.idleExpiry(progressAt)
		if intent.ExpiresAt.Before(deadline) {
			intent.ExpiresAt = deadline
		}
		if err := r.SaveBlobIntent(ctx, &intent); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := r.GetBlobIntent(ctx, intentID)
			if readErr == nil && !latest.LastProgressAt.Before(progressAt) {
				return nil
			}
			continue
		} else {
			return err
		}
	}
	return storage.ErrUnavailable
}

func (r *Records) verifyBlobDemand(ctx context.Context, key, intentID, demandID string) error {
	object, err := r.store.Get(ctx, key)
	if err != nil {
		return err
	}
	var link struct {
		SchemaVersion int    `json:"schemaVersion"`
		IntentID      string `json:"intentID"`
		DemandID      string `json:"demandID"`
	}
	if err := decodeRecord(object, 1<<20, &link); err != nil {
		return err
	}
	if link.SchemaVersion != solidificationRecordSchema || link.IntentID != intentID || link.DemandID != demandID {
		return errors.New("existing blob demand link conflicts with the requested identity")
	}
	return nil
}

func (r *Records) ListBlobDemands(ctx context.Context, intentID, token string, limit int) ([]string, string, error) {
	records, next, err := r.ListBlobDemandRecords(ctx, intentID, token, limit)
	if err != nil {
		return nil, "", err
	}
	result := make([]string, 0, len(records))
	for _, record := range records {
		result = append(result, record.DemandID)
	}
	return result, next, nil
}

type BlobDemand struct {
	SchemaVersion int       `json:"schemaVersion"`
	IntentID      string    `json:"intentID"`
	DemandID      string    `json:"demandID"`
	RootTaskID    string    `json:"rootTaskID,omitempty"`
	ScopeID       string    `json:"scopeID,omitempty"`
	SessionID     string    `json:"sessionID,omitempty"`
	ReservedBytes int64     `json:"reservedBytes,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (r *Records) ListRootBlobDemandRecords(ctx context.Context, rootTaskID, token string, limit int) ([]BlobDemand, string, error) {
	if !validID(rootTaskID) {
		return nil, "", errors.New("blob root task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := r.store.List(ctx, rootBlobDemandPrefix(rootTaskID), token, limit)
	if err != nil {
		return nil, "", err
	}
	result := make([]BlobDemand, 0, len(entries))
	for _, entry := range entries {
		object, err := r.store.Get(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var demand BlobDemand
		if err := decodeRecord(object, 1<<20, &demand); err != nil {
			return nil, "", err
		}
		if demand.SchemaVersion != solidificationRecordSchema || demand.RootTaskID != rootTaskID || !validID(demand.IntentID) || !validID(demand.DemandID) || demand.ScopeID == "" || !validID(demand.SessionID) || demand.ReservedBytes < 0 {
			return nil, "", errors.New("root blob demand index is invalid")
		}
		result = append(result, demand)
	}
	return result, next, nil
}

func (r *Records) RemoveBlobDemand(ctx context.Context, demand BlobDemand) error {
	if !validID(demand.IntentID) || !validID(demand.DemandID) || demand.RootTaskID != "" && !validID(demand.RootTaskID) {
		return errors.New("blob demand identity is invalid")
	}
	key := blobDemandKey(demand.IntentID, demand.DemandID)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := r.readBlobDemand(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.RootTaskID != demand.RootTaskID {
			return storage.ErrConditionFailed
		}
		if err := r.store.DeleteIfMatch(ctx, key, version); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrUnavailable
}

func (r *Records) DeleteRootBlobDemandRecord(ctx context.Context, demand BlobDemand) error {
	if !validID(demand.RootTaskID) || !validID(demand.IntentID) || !validID(demand.DemandID) {
		return errors.New("root blob demand identity is invalid")
	}
	key := rootBlobDemandIndexKey(demand.RootTaskID, demand.IntentID, demand.DemandID)
	for attempt := 0; attempt < 16; attempt++ {
		object, err := r.store.Get(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var current BlobDemand
		if err := decodeRecord(object, 1<<20, &current); err != nil {
			return err
		}
		if current.SchemaVersion != solidificationRecordSchema || current.RootTaskID != demand.RootTaskID || current.IntentID != demand.IntentID || current.DemandID != demand.DemandID {
			return errors.New("root blob demand index conflicts with its identity")
		}
		if err := r.store.DeleteIfMatch(ctx, key, object.Version); errors.Is(err, storage.ErrConditionFailed) {
			continue
		} else if errors.Is(err, storage.ErrNotFound) {
			return nil
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrUnavailable
}

func (r *Records) ListBlobDemandRecords(ctx context.Context, intentID, token string, limit int) ([]BlobDemand, string, error) {
	if !validID(intentID) {
		return nil, "", errors.New("blob intent ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := r.store.List(ctx, "solidification/blobs/owners/"+intentID+"/", token, limit)
	if err != nil {
		return nil, "", err
	}
	result := make([]BlobDemand, 0, len(entries))
	for _, entry := range entries {
		link, _, getErr := r.readBlobDemand(ctx, entry.Key)
		if errors.Is(getErr, storage.ErrNotFound) {
			continue
		}
		if getErr != nil {
			return nil, "", getErr
		}
		if link.IntentID != intentID || !validID(link.DemandID) || link.RootTaskID != "" && !validID(link.RootTaskID) || link.ReservedBytes < 0 || link.ScopeID == "" && (link.RootTaskID != "" || link.SessionID != "" || link.ReservedBytes != 0) || link.ScopeID != "" && !validID(link.SessionID) {
			return nil, "", errors.New("blob demand link is invalid")
		}
		result = append(result, link)
	}
	return result, next, nil
}

func (r *Records) readBlobDemand(ctx context.Context, key string) (BlobDemand, string, error) {
	object, err := r.store.Get(ctx, key)
	if err != nil {
		return BlobDemand{}, "", err
	}
	var link BlobDemand
	if err := decodeRecord(object, 1<<20, &link); err != nil {
		return BlobDemand{}, "", err
	}
	if link.SchemaVersion != solidificationRecordSchema || !validID(link.IntentID) || !validID(link.DemandID) || link.RootTaskID != "" && !validID(link.RootTaskID) || link.ReservedBytes < 0 {
		return BlobDemand{}, "", errors.New("blob demand link is invalid")
	}
	return link, object.Version, nil
}

func readBlobIntentIndex(ctx context.Context, store storage.Store, key string) (blobIntentIndex, storage.Entry, error) {
	object, err := store.Get(ctx, key)
	if err != nil {
		return blobIntentIndex{}, storage.Entry{}, err
	}
	var index blobIntentIndex
	if err := decodeRecord(object, 1<<20, &index); err != nil {
		return blobIntentIndex{}, storage.Entry{}, err
	}
	if index.SchemaVersion != solidificationRecordSchema || !validID(index.IntentID) {
		return blobIntentIndex{}, storage.Entry{}, errors.New("blob intent index is invalid")
	}
	return index, storage.Entry{Version: object.Version}, nil
}

type blobIntentIndex struct {
	SchemaVersion int    `json:"schemaVersion"`
	IntentID      string `json:"intentID"`
}

func (r *Records) writeBlobIntentIndex(ctx context.Context, key, intentID, version string) error {
	data, err := json.Marshal(blobIntentIndex{SchemaVersion: solidificationRecordSchema, IntentID: intentID})
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: true}
	if version != "" {
		options = storage.PutOptions{IfMatch: version}
	}
	_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	return err
}

func (r *Records) discardBlobIntent(intent BlobIntent, demandID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if demandID != "" {
		key := blobDemandKey(intent.ID, demandID)
		if entry, err := r.store.Head(ctx, key); err == nil {
			_ = r.store.DeleteIfMatch(ctx, key, entry.Version)
		}
	}
	if intent.version != "" {
		_ = r.store.DeleteIfMatch(ctx, blobIntentKey(intent.ID), intent.version)
	}
}

func sameBlobIntentIdentity(intent BlobIntent, identity BlobIdentity, round, generation uint64) bool {
	return intent.Identity == identity && intent.Round == round && intent.Generation == generation
}

func blobIntentKey(id string) string { return "solidification/blobs/intents/" + id + ".json" }

func blobDemandKey(intentID, demandID string) string {
	return "solidification/blobs/owners/" + intentID + "/" + demandID + ".json"
}
