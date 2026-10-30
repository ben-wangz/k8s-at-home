package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type notificationWorkLink struct {
	IntentID  string    `json:"intentID"`
	WorkID    string    `json:"workID"`
	IndexedAt time.Time `json:"indexedAt"`
}

func notificationIndexPrefix(intentID string) string {
	return diagnostics.Prefix + "notification-work/" + intentID + "/"
}

func (s *Service) indexNotificationJob(ctx context.Context, job workqueue.Job) error {
	if job.Kind != "notify" {
		return nil
	}
	var source blobNotification
	if json.Unmarshal(job.Payload, &source) != nil || !validGateID(source.IntentID) {
		return nil
	}
	key := notificationIndexPrefix(source.IntentID) + job.ID + ".json"
	prior, _, err := inventory.Read[notificationWorkLink](ctx, s.store, key)
	if err == nil && prior.IntentID == source.IntentID && prior.WorkID == job.ID {
		return nil
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	return inventory.Update(ctx, s.store, key, func(value *notificationWorkLink) error {
		*value = notificationWorkLink{IntentID: source.IntentID, WorkID: job.ID, IndexedAt: time.Now().UTC()}
		return nil
	})
}

func notificationMatches(source blobNotification, intent tasks.BlobIntent) bool {
	return source.IntentID == intent.ID && source.State == intent.State && source.Generation == intent.Generation && source.CacheVersion == intent.CacheVersion && source.DemandRevision == intent.DemandRevision
}

func (s *Service) cleanupNotificationIndex(ctx context.Context, entry storage.Entry) error {
	link, _, err := inventory.Read[notificationWorkLink](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	_, err = (workqueue.Queue{Store: s.store}).Read(ctx, "notify", link.WorkID)
	if errors.Is(err, storage.ErrNotFound) {
		return s.deleteDiagnosticObservation(ctx, entry)
	}
	return err
}
