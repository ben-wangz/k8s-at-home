package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

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
	recordIntentEvent(&intent)
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
	recordIntentEvent(intent)
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
