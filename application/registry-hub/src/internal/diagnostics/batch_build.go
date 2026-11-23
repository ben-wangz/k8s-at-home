package diagnostics

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"registry-hub/internal/storage"
)

func buildEventBatch(events []Event, critical bool) (EventBatch, []byte, EventBatchReservation, error) {
	if len(events) == 0 || len(events) > BatchMaxEvents {
		return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
	}
	unique := make([]Event, 0, len(events))
	seen := map[string][]byte{}
	for _, event := range events {
		if event.ID == "" || event.OccurredAt.IsZero() {
			return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return EventBatch{}, nil, EventBatchReservation{}, err
		}
		if prior, ok := seen[event.ID]; ok {
			if !bytes.Equal(prior, encoded) {
				return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
			}
			continue
		}
		seen[event.ID] = encoded
		unique = append(unique, event)
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].OccurredAt.Equal(unique[j].OccurredAt) {
			return unique[i].ID < unique[j].ID
		}
		return unique[i].OccurredAt.Before(unique[j].OccurredAt)
	})
	date := unique[0].OccurredAt.UTC().Format("20060102")
	created := unique[0].OccurredAt.UTC()
	for _, event := range unique {
		if event.OccurredAt.UTC().Format("20060102") != date {
			return EventBatch{}, nil, EventBatchReservation{}, fmt.Errorf("event batch spans UTC partitions: %w", storage.ErrCorruptObject)
		}
		if event.OccurredAt.After(created) {
			created = event.OccurredAt.UTC()
		}
	}
	content, err := json.Marshal(unique)
	if err != nil {
		return EventBatch{}, nil, EventBatchReservation{}, err
	}
	contentHash := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(contentHash[:])
	id := ID("event-batch", date, contentSHA)
	batch := EventBatch{SchemaVersion: 2, ID: id, Date: date, ContentSHA256: contentSHA, CreatedAt: created, Events: unique}
	data, err := json.Marshal(batch)
	if err != nil {
		return EventBatch{}, nil, EventBatchReservation{}, err
	}
	if len(data) > BatchMaxBytes {
		return EventBatch{}, nil, EventBatchReservation{}, storage.ErrCorruptObject
	}
	return batch, data, EventBatchReservation{Date: date, Bytes: int64(len(data)), Critical: critical}, nil
}
