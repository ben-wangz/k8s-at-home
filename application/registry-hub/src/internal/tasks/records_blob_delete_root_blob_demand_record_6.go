package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

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
