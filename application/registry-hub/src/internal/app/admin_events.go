package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type eventPage struct {
	diagnosticPage[diagnostics.Event]
	RetentionSeconds int64                      `json:"retentionSeconds"`
	DailyBytesLimit  int64                      `json:"dailyBytesLimit"`
	Coverage         string                     `json:"coverage"`
	HistoryBefore    time.Time                  `json:"historyBefore"`
	Gaps             diagnosticHistoryCoverage  `json:"gaps"`
	TodayBudget      *diagnostics.HistoryBudget `json:"todayBudget"`
	ReadErrors       map[string]string          `json:"readErrors,omitempty"`
	ProcessDropped   uint64                     `json:"processDropped"`
}

var eventQueryFields = []string{"component", "operation", "imageID", "authority", "repository", "digest", "taskID", "intentID", "transferID", "workID", "runID", "attemptID", "parentAttemptID", "attemptLayer", "result", "errorClass", "timeoutScope", "since", "before"}

func (s *Service) serveEvents(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	values, cursor, limit, err := s.diagnosticQuery(request, eventQueryFields...)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	var since, before time.Time
	for _, filter := range []struct {
		name   string
		target *time.Time
	}{{"since", &since}, {"before", &before}} {
		if raw := values.Get(filter.name); raw != "" {
			*filter.target, err = time.Parse(time.RFC3339, raw)
			if err != nil {
				writeInventoryError(writer, inventory.ErrInvalid)
				return
			}
		}
	}
	entries, next, err := storage.Scan(ctx, s.store, diagnostics.Prefix+"events/", cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := eventPage{diagnosticPage: diagnosticPage[diagnostics.Event]{Items: []diagnostics.Event{}, ObservedAt: time.Now().UTC(), Consistency: "immutable events; live enumeration", Scanned: len(entries)}, RetentionSeconds: int64(diagnostics.Retention.Seconds()), DailyBytesLimit: diagnostics.DailyBytesLimit, Coverage: "bounded durable source outboxes plus best-effort transport attempts and reconciliation observations; process interruption, collection failure and daily capacity can leave gaps", HistoryBefore: time.Now().UTC().Add(-diagnostics.Retention)}
	page.ReadErrors = map[string]string{}
	page.ProcessDropped = s.diagnosticEventsDropped.Load()
	page.Gaps, _, err = inventory.Read[diagnosticHistoryCoverage](ctx, s.store, diagnostics.Prefix+"coverage.json")
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		page.ReadErrors["gaps"] = diagnosticReason(err)
	}
	budget, _, err := inventory.Read[diagnostics.HistoryBudget](ctx, s.store, diagnostics.Prefix+"event-budgets/"+time.Now().UTC().Format("20060102")+".json")
	if err == nil {
		page.TodayBudget = &budget
	} else if !errors.Is(err, storage.ErrNotFound) {
		page.ReadErrors["budget"] = diagnosticReason(err)
	}
	for _, entry := range entries {
		event, _, err := inventory.Read[diagnostics.Event](ctx, s.store, entry.Key)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if !since.IsZero() && event.OccurredAt.Before(since) || !before.IsZero() && !event.OccurredAt.Before(before) || event.OccurredAt.Before(page.HistoryBefore) {
			continue
		}
		identity := inventory.Identity{Authority: event.Authority, Repository: event.Repository}
		imageID := event.ImageID
		if imageID == "" {
			imageID = identity.ContentID(event.Digest)
		}
		fields := map[string]string{"component": event.Component, "operation": event.Operation, "imageID": imageID, "authority": event.Authority, "repository": event.Repository, "digest": event.Digest, "taskID": event.TaskID, "intentID": event.IntentID, "transferID": event.TransferID, "workID": event.WorkID, "runID": event.RunID, "attemptID": event.AttemptID, "parentAttemptID": event.ParentAttemptID, "attemptLayer": event.AttemptLayer, "result": event.Result, "errorClass": event.ErrorClass, "timeoutScope": event.TimeoutScope}
		matched := true
		for key, value := range fields {
			if values.Get(key) != "" && values.Get(key) != value {
				matched = false
			}
		}
		if matched {
			page.Items = append(page.Items, event)
		}
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
