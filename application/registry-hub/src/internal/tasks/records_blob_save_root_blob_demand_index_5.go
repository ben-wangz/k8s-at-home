package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

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
		intent.DemandRevision++
		intent.WakeAcknowledged = false
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
