package workqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const completionMarkerPrefix = Prefix + "idempotency/"

type CompletionMarker struct {
	SchemaVersion int             `json:"schemaVersion"`
	Kind          string          `json:"kind"`
	ID            string          `json:"id"`
	PayloadSHA256 string          `json:"payloadSHA256"`
	Disposition   string          `json:"disposition"`
	Superseded    bool            `json:"superseded,omitempty"`
	FinalSummary  json.RawMessage `json:"finalSummary,omitempty"`
	CreatedAt     time.Time       `json:"createdAt"`
	CompletedAt   time.Time       `json:"completedAt,omitempty"`
	UpdatedAt     time.Time       `json:"updatedAt"`
}

func CompletionMarkerKey(kind, id string) string {
	return completionMarkerPrefix + kind + "/" + id + ".json"
}

func (q Queue) PersistCompletionMarker(ctx context.Context, job Job) error {
	return q.writeCompletionMarker(ctx, job)
}

func (q Queue) completionMarker(ctx context.Context, kind, id string, payload []byte) (Job, bool, error) {
	marker, _, err := inventory.Read[CompletionMarker](ctx, q.Store, CompletionMarkerKey(kind, id))
	if errors.Is(err, storage.ErrNotFound) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if marker.SchemaVersion != 1 || marker.Kind != kind || marker.ID != id || marker.Disposition != "done" ||
		marker.PayloadSHA256 != payloadFingerprint(payload) {
		return Job{}, false, storage.ErrCorruptObject
	}
	return Job{
		SchemaVersion: 1, ID: marker.ID, Kind: marker.Kind, Payload: append(json.RawMessage(nil), payload...),
		State: "done", Superseded: marker.Superseded, FinalSummary: append(json.RawMessage(nil), marker.FinalSummary...),
		CreatedAt: marker.CreatedAt, CompletedAt: marker.CompletedAt,
	}, true, nil
}

func (q Queue) writeCompletionMarker(ctx context.Context, job Job) error {
	if job.State != "done" || job.ID == "" || job.Kind == "" {
		return storage.ErrConditionFailed
	}
	completedAt := job.CompletedAt
	if completedAt.IsZero() {
		completedAt = job.AttemptFinishedAt
	}
	marker := CompletionMarker{
		SchemaVersion: 1, Kind: job.Kind, ID: job.ID, PayloadSHA256: payloadFingerprint(job.Payload),
		Disposition: "done", Superseded: job.Superseded, FinalSummary: append(json.RawMessage(nil), job.FinalSummary...),
		CreatedAt: job.CreatedAt, CompletedAt: completedAt, UpdatedAt: time.Now().UTC(),
	}
	key := CompletionMarkerKey(job.Kind, job.ID)
	for attempt := 0; attempt < 16; attempt++ {
		data, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if err := inventory.ValidateWriteGuard(ctx); err != nil {
			return err
		}
		_, err = q.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
			return err
		}
		stored, version, readErr := inventory.Read[CompletionMarker](ctx, q.Store, key)
		if readErr != nil {
			return errors.Join(err, readErr)
		}
		if stored.SchemaVersion != 1 || stored.Kind != job.Kind || stored.ID != job.ID || stored.Disposition != "done" ||
			stored.PayloadSHA256 != marker.PayloadSHA256 || stored.Superseded != marker.Superseded {
			return storage.ErrCorruptObject
		}
		if len(stored.FinalSummary) > 0 && len(marker.FinalSummary) > 0 && !bytes.Equal(stored.FinalSummary, marker.FinalSummary) {
			return storage.ErrCorruptObject
		}
		if len(stored.FinalSummary) > 0 || len(marker.FinalSummary) == 0 {
			return nil
		}
		stored.FinalSummary = marker.FinalSummary
		stored.UpdatedAt = time.Now().UTC()
		updated, marshalErr := json.Marshal(stored)
		if marshalErr != nil {
			return marshalErr
		}
		_, err = q.Store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: version})
		if errors.Is(err, storage.ErrConditionFailed) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			actual, _, readErr := inventory.Read[CompletionMarker](ctx, q.Store, key)
			if readErr == nil && actual.PayloadSHA256 == marker.PayloadSHA256 && bytes.Equal(actual.FinalSummary, marker.FinalSummary) {
				return nil
			}
			return errors.Join(err, readErr)
		}
		return err
	}
	return storage.ErrUnavailable
}

func payloadFingerprint(payload []byte) string {
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}
