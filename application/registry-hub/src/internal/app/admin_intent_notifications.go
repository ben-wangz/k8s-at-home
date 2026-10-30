package app

import (
	"context"
	"encoding/json"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

type notificationWorkView struct {
	responsibleWork
	ExcludedRootID       string `json:"excludedRootID,omitempty"`
	MatchesCurrentIntent bool   `json:"matchesCurrentIntent"`
	TargetsURL           string `json:"targetsURL"`
	ReadReason           string `json:"readReason,omitempty"`
}

type notificationWorkPage struct {
	diagnosticPage[notificationWorkView]
	Coverage string `json:"coverage"`
}

func (s *Service) serveIntentNotifications(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/blob-intents/"), "/")
	if len(parts) == 4 && parts[1] == "notifications" && parts[3] == "targets" && validGateID(parts[0]) && inventory.ValidID(parts[2]) {
		s.serveNotificationTargets(writer, request, ctx, parts[0], parts[2])
		return
	}
	if len(parts) != 2 || !validGateID(parts[0]) || parts[1] != "notifications" {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	_, cursor, limit, err := s.diagnosticQuery(request)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	intent, err := s.records.GetBlobIntent(ctx, parts[0])
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	entries, next, err := storage.Scan(ctx, s.store, notificationIndexPrefix(intent.ID), cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := notificationWorkPage{diagnosticPage: diagnosticPage[notificationWorkView]{Items: []notificationWorkView{}, ObservedAt: time.Now().UTC(), Consistency: "live indexed notification jobs and receipts; concurrent inserts may appear next traversal", Scanned: len(entries)}, Coverage: "current and excluded-root variants indexed at enqueue; history sweeps recover older jobs; no global completeness watermark"}
	for _, entry := range entries {
		link, _, err := inventory.Read[notificationWorkLink](ctx, s.store, entry.Key)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "notify", link.WorkID)
		if err != nil {
			page.Items = append(page.Items, notificationWorkView{responsibleWork: responsibleWork{ID: link.WorkID, Kind: "notify", State: "unknown"}, ReadReason: diagnosticReason(err)})
			continue
		}
		var source blobNotification
		if json.Unmarshal(job.Payload, &source) != nil || source.IntentID != intent.ID {
			writeInventoryError(writer, storage.ErrCorruptObject)
			return
		}
		page.Items = append(page.Items, notificationWorkView{responsibleWork: *notificationWork(job), ExcludedRootID: source.ExcludedRootID, MatchesCurrentIntent: notificationMatches(source, intent), TargetsURL: "/api/v1/blob-intents/" + intent.ID + "/notifications/" + job.ID + "/targets"})
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
