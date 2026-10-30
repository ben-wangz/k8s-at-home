package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

type notificationTarget struct {
	RootTaskID string           `json:"rootTaskID"`
	Source     string           `json:"source"`
	Excluded   bool             `json:"excluded"`
	Work       *responsibleWork `json:"work,omitempty"`
	State      string           `json:"state"`
	ReadReason string           `json:"readReason,omitempty"`
}

type notificationTargetPage struct {
	diagnosticPage[notificationTarget]
	Scope string `json:"scope"`
}

func (s *Service) serveNotificationTargets(writer http.ResponseWriter, request *http.Request, ctx context.Context, intentID, workID string) {
	_, cursor, limit, err := s.diagnosticQuery(request)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "notify", workID)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	var source blobNotification
	if json.Unmarshal(job.Payload, &source) != nil || source.IntentID != intentID {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if cursor.Phase < 0 || cursor.Phase > 1 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	prefix, sourceKind := "solidification/blobs/owners/"+intentID+"/", "blob_demand"
	if cursor.Phase == 1 {
		data, _ := json.Marshal(source.Identity.Digest)
		hash := sha256.Sum256(data)
		prefix, sourceKind = "solidification/cache-protection/digests/"+hex.EncodeToString(hash[:])+"/", "cache_protection_reference"
	}
	entries, next, err := storage.Scan(ctx, s.store, prefix, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := notificationTargetPage{diagnosticPage: diagnosticPage[notificationTarget]{Items: []notificationTarget{}, ObservedAt: time.Now().UTC(), Consistency: "live target candidates and wake work; source references can change while paginating", Scanned: len(entries)}, Scope: "owner and protection reference candidates, possibly repeated roots; confirmedTargets counts reference confirmations, not distinct roots"}
	for _, entry := range entries {
		rootID, err := s.notificationRoot(ctx, entry.Key, source, cursor.Phase)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if rootID == "" {
			continue
		}
		target := notificationTarget{RootTaskID: rootID, Source: sourceKind, Excluded: rootID == source.ExcludedRootID, State: "not_enqueued"}
		if target.Excluded {
			target.State = "excluded"
		} else {
			child, readErr := (workqueue.Queue{Store: s.store}).Read(ctx, "wake_root", workqueue.ID("wake_root", job.ID+"/"+rootID))
			if readErr == nil {
				target.Work = notificationWork(child)
				target.State = target.Work.State
			} else if !errors.Is(readErr, storage.ErrNotFound) {
				target.State, target.ReadReason = "unknown", diagnosticReason(readErr)
			}
		}
		page.Items = append(page.Items, target)
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	} else if cursor.Phase == 0 {
		cursor.Phase, cursor.Storage = 1, ""
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
