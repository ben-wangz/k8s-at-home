package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"time"
)

type blobIntentView struct {
	tasks.BlobIntent
	TaskURL              string             `json:"taskURL"`
	EventsURL            string             `json:"eventsURL"`
	ActivationWorkerURL  string             `json:"activationWorkerURL"`
	Demands              []tasks.BlobDemand `json:"demands,omitempty"`
	DemandsNextToken     string             `json:"demandsNextToken,omitempty"`
	TaskExists           *bool              `json:"taskExists"`
	NotificationWork     *responsibleWork   `json:"notificationWork,omitempty"`
	NotificationCoverage string             `json:"notificationCoverage"`
	NotificationsURL     string             `json:"notificationsURL"`
	ReadErrors           map[string]string  `json:"readErrors,omitempty"`
	DemandImages         map[string]string  `json:"demandImages,omitempty"`
	ErrorSummary         string             `json:"errorSummary,omitempty"`
}

func intentView(intent tasks.BlobIntent) blobIntentView {
	intent.ErrorMessage = ""
	intent.DiagnosticOutbox = nil
	return blobIntentView{BlobIntent: intent, EventsURL: "/api/v1/events?intentID=" + intent.ID, ActivationWorkerURL: "/api/v1/workers/activation", ReadErrors: map[string]string{}, NotificationCoverage: "current intent revision with no excluded root; use notificationsURL for indexed variants", NotificationsURL: "/api/v1/blob-intents/" + intent.ID + "/notifications"}
}

func (s *Service) diagnosticIntent(ctx context.Context, intent tasks.BlobIntent) blobIntentView {
	view := intentView(intent)
	_, err := s.tasks.Get(ctx, intent.ID)
	if err == nil || errors.Is(err, storage.ErrNotFound) {
		exists := err == nil
		view.TaskExists = &exists
		if exists {
			view.TaskURL = "/api/v1/tasks/" + intent.ID
		}
	} else {
		view.ReadErrors["task"] = diagnosticReason(err)
	}
	view.NotificationWork, err = s.intentNotification(ctx, intent)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		view.ReadErrors["notification"] = diagnosticReason(err)
	}
	if intent.ErrorClass != "" {
		view.ErrorSummary = "blob activation: " + stableDiagnosticError(intent.ErrorClass)
	}
	return view
}

func (s *Service) serveBlobIntents(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if request.URL.Path != "/api/v1/blob-intents" {
		id := strings.TrimPrefix(request.URL.Path, "/api/v1/blob-intents/")
		if !validGateID(id) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		_, cursor, limit, err := s.diagnosticQuery(request)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		intent, err := s.records.GetBlobIntent(ctx, id)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		view := s.diagnosticIntent(ctx, intent)
		view.Demands, cursor.Storage, err = s.records.ListBlobDemandRecords(ctx, id, cursor.Storage, limit)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if cursor.Storage != "" {
			view.DemandsNextToken = s.diagnosticToken(cursor)
		}
		view.DemandImages = map[string]string{}
		for _, demand := range view.Demands {
			if demand.RootTaskID == "" {
				continue
			}
			root, readErr := s.tasks.Get(ctx, demand.RootTaskID)
			if readErr == nil {
				view.DemandImages[demand.DemandID] = inventoryIdentity(root.Authority, root.Repository, root.Reference).ID()
			} else {
				view.ReadErrors["demand/"+demand.DemandID] = diagnosticReason(readErr)
			}
		}
		writeJSON(writer, http.StatusOK, view)
		return
	}
	values, cursor, limit, err := s.diagnosticQuery(request, "state", "authority", "repository", "digest", "errorClass", "round")
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	items, next, err := s.records.ListBlobIntents(ctx, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := diagnosticPage[blobIntentView]{Items: []blobIntentView{}, ObservedAt: time.Now().UTC(), Consistency: "live", Scanned: len(items)}
	for _, intent := range items {
		fields := map[string]string{"state": string(intent.State), "authority": intent.Identity.Authority, "repository": intent.Identity.Repository, "digest": intent.Identity.Digest, "errorClass": intent.ErrorClass, "round": strconv.FormatUint(intent.Round, 10)}
		match := true
		for key, value := range fields {
			if values.Get(key) != "" && values.Get(key) != value {
				match = false
			}
		}
		if match {
			page.Items = append(page.Items, s.diagnosticIntent(ctx, intent))
		}
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
