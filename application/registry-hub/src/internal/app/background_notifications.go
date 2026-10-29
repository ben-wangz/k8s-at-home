package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type blobNotification struct {
	IntentID       string                `json:"intentID"`
	Identity       tasks.BlobIdentity    `json:"identity"`
	State          tasks.BlobIntentState `json:"state"`
	Generation     uint64                `json:"generation"`
	CacheVersion   string                `json:"cacheVersion"`
	DemandRevision uint64                `json:"demandRevision"`
	ExcludedRootID string                `json:"excludedRootID"`
}

type notificationProgress struct {
	Phase  int              `json:"phase"`
	Cursor inventory.Cursor `json:"cursor"`
}

func (s *Service) deferBlobNotification(ctx context.Context, intentID, excluded string) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	intent, err := s.records.GetBlobIntent(bounded, intentID)
	if err != nil {
		return err
	}
	source := blobNotification{IntentID: intent.ID, Identity: intent.Identity, State: intent.State, Generation: intent.Generation, CacheVersion: intent.CacheVersion, DemandRevision: intent.DemandRevision, ExcludedRootID: excluded}
	key := inventory.ID("notify", intent.ID, string(intent.State), intent.CacheVersion, strconv.FormatUint(intent.DemandRevision, 10), excluded)
	_, err = (workqueue.Queue{Store: s.store}).Enqueue(bounded, "notify", key, source, intent.UpdatedAt)
	return err
}

func (s *Service) notificationStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var source blobNotification
	var p notificationProgress
	if json.Unmarshal(job.Payload, &source) != nil || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &p) != nil {
		return p, false, storage.ErrCorruptObject
	}
	current, err := s.records.GetBlobIntent(ctx, source.IntentID)
	if errors.Is(err, storage.ErrNotFound) {
		return p, true, nil
	}
	if err != nil {
		return p, false, err
	}
	if current.DemandRevision != source.DemandRevision || current.State != source.State || current.CacheVersion != source.CacheVersion || current.Generation != source.Generation {
		return p, true, nil
	}
	if p.Phase == 4 {
		if current.WakeAcknowledged {
			return p, true, nil
		}
		if err := (workqueue.Queue{Store: s.store}).Validate(ctx, *job); err != nil {
			return p, false, err
		}
		current.WakeAcknowledged = true
		return p, true, s.records.SaveBlobIntent(ctx, &current)
	}
	prefix := "solidification/blobs/owners/" + source.IntentID + "/"
	if p.Phase%2 == 1 {
		data, _ := json.Marshal(source.Identity.Digest)
		hash := sha256.Sum256(data)
		prefix = "solidification/cache-protection/digests/" + hex.EncodeToString(hash[:]) + "/"
	}
	entry, ok, err := p.Cursor.Peek(ctx, s.store, prefix, 32)
	if err != nil {
		return p, false, err
	}
	if ok {
		rootID, err := s.notificationRoot(ctx, entry.Key, source, p.Phase%2)
		if errors.Is(err, storage.ErrNotFound) {
			rootID, err = "", nil
		}
		if err != nil {
			return p, false, err
		}
		if rootID != "" && rootID != source.ExcludedRootID {
			childID := workqueue.ID("wake_root", job.ID+"/"+rootID)
			q := workqueue.Queue{Store: s.store}
			if p.Phase < 2 {
				_, err = q.Enqueue(ctx, "wake_root", job.ID+"/"+rootID, rootID, job.CreatedAt)
			} else {
				child, readErr := q.Read(ctx, "wake_root", childID)
				if errors.Is(readErr, storage.ErrNotFound) {
					_, err = q.Enqueue(ctx, "wake_root", job.ID+"/"+rootID, rootID, job.CreatedAt)
					return p, false, err
				}
				err = readErr
				if err == nil && child.State != "done" {
					return p, false, nil
				}
			}
			if err != nil {
				return p, false, err
			}
		}
		p.Cursor.Advance()
	}
	if p.Cursor.Done {
		p.Cursor = inventory.Cursor{}
		p.Phase++
	}
	return p, false, nil
}

func (s *Service) notificationRoot(ctx context.Context, key string, source blobNotification, phase int) (string, error) {
	if phase == 0 {
		demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, key)
		return demand.RootTaskID, err
	}
	reference, _, err := inventory.Read[tasks.CacheProtectionReference](ctx, s.store, key)
	if err != nil {
		return "", err
	}
	if reference.Authority != source.Identity.Authority || reference.Repository != source.Identity.Repository {
		return "", nil
	}
	return reference.TaskID, nil
}

func (s *Service) wakeRootStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var root string
	if json.Unmarshal(job.Payload, &root) != nil {
		return nil, false, storage.ErrCorruptObject
	}
	err := s.wakeBlobDemandRoot(ctx, root)
	return nil, err == nil, err
}
