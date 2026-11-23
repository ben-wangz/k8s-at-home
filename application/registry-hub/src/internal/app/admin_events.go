package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type eventPage struct {
	diagnosticPage[diagnostics.Event]
	RetentionSeconds int64                                `json:"retentionSeconds"`
	DailyBytesLimit  int64                                `json:"dailyBytesLimit"`
	Coverage         string                               `json:"coverage"`
	HistoryBefore    time.Time                            `json:"historyBefore"`
	Gaps             diagnosticHistoryCoverage            `json:"gaps"`
	TodayBudget      *diagnostics.HistoryBudget           `json:"todayBudget"`
	TodayBatchBudget *diagnostics.EventBatchBudgetSummary `json:"todayBatchBudget,omitempty"`
	ReadErrors       map[string]string                    `json:"readErrors,omitempty"`
	ProcessDropped   uint64                               `json:"processDropped"`
	SourceWakeDrops  uint64                               `json:"sourceWakeDrops"`
	SourceWakeErrors uint64                               `json:"sourceWakeErrors"`
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
	if cursor.Phase < 0 || cursor.Phase > 2 || cursor.BatchOffset < 0 || cursor.BatchOffset >= diagnostics.BatchMaxEvents ||
		cursor.BatchKey != "" && !strings.HasPrefix(cursor.BatchKey, diagnostics.BatchPrefix) ||
		cursor.BatchKey == "" && (cursor.BatchOffset != 0 || cursor.BatchNext != "") {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	retention := s.diagnosticsConfig().EventRetention
	page := eventPage{diagnosticPage: diagnosticPage[diagnostics.Event]{Items: []diagnostics.Event{}, ObservedAt: time.Now().UTC(), Consistency: "immutable v1 events and v2 batches; live enumeration"}, RetentionSeconds: int64(retention.Seconds()), DailyBytesLimit: diagnostics.DailyBytesLimit, Coverage: "bounded durable source outboxes plus best-effort transport attempts and reconciliation observations; process interruption, collection failure and capacity policy can leave reported gaps", HistoryBefore: time.Now().UTC().Add(-retention)}
	page.ReadErrors = map[string]string{}
	page.ProcessDropped = s.diagnosticEventsDropped.Load()
	page.SourceWakeDrops = s.diagnosticSourceDrops.Load()
	page.SourceWakeErrors = s.diagnosticSourceErrors.Load()
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
	if summary, err := (diagnostics.BatchLog{Store: s.store, Policy: s.diagnosticBatchPolicy(), Retention: retention}).Summary(ctx, time.Now().UTC().Format("20060102")); err == nil {
		page.TodayBatchBudget = &summary
	} else if !errors.Is(err, storage.ErrNotFound) {
		page.ReadErrors["batchBudget"] = diagnosticReason(err)
	}
	if cursor.Phase == 0 {
		entries, next, err := storage.Scan(ctx, s.store, diagnostics.Prefix+"events/", cursor.Storage, limit)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		page.Scanned = len(entries)
		for _, entry := range entries {
			event, _, err := inventory.Read[diagnostics.Event](ctx, s.store, entry.Key)
			if err != nil {
				writeInventoryError(writer, err)
				return
			}
			if diagnosticEventMatches(event, values, since, before, page.HistoryBefore) {
				page.Items = append(page.Items, event)
			}
		}
		if next != "" {
			cursor.Storage = next
			page.NextToken = s.diagnosticToken(cursor)
			writeJSON(writer, http.StatusOK, page)
			return
		}
		cursor.Phase, cursor.Storage = 1, ""
		if len(page.Items) >= limit {
			entries, _, err := storage.Scan(ctx, s.store, diagnostics.BatchPrefix, "", 1)
			if err != nil {
				writeInventoryError(writer, err)
				return
			}
			if len(entries) > 0 {
				page.NextToken = s.diagnosticToken(cursor)
			}
			writeJSON(writer, http.StatusOK, page)
			return
		}
	}
	if cursor.Phase == 1 {
		if err := s.appendDiagnosticBatchPage(ctx, &page, values, since, before, &cursor, limit); err != nil {
			writeInventoryError(writer, err)
			return
		}
	}
	if cursor.Phase < 2 && (cursor.BatchKey != "" || cursor.Storage != "") {
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
	return
}

func (s *Service) appendDiagnosticBatchPage(ctx context.Context, page *eventPage, values url.Values, since, before time.Time, cursor *diagnosticCursor, limit int) error {
	for len(page.Items) < limit && cursor.Phase == 1 {
		if cursor.BatchKey == "" {
			entries, next, err := storage.Scan(ctx, s.store, diagnostics.BatchPrefix, cursor.Storage, 1)
			if err != nil {
				return err
			}
			if len(entries) == 0 {
				cursor.Storage, cursor.Phase = "", 2
				break
			}
			cursor.BatchKey, cursor.BatchNext, cursor.BatchOffset = entries[0].Key, next, 0
		}
		batch, _, err := inventory.Read[diagnostics.EventBatch](ctx, s.store, cursor.BatchKey)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return inventory.ErrExpired
			}
			return err
		}
		if err := diagnostics.VerifyEventBatch(batch, cursor.BatchKey); err != nil {
			return err
		}
		for cursor.BatchOffset < len(batch.Events) {
			event := batch.Events[cursor.BatchOffset]
			cursor.BatchOffset++
			page.Scanned++
			if diagnosticEventMatches(event, values, since, before, page.HistoryBefore) {
				page.Items = append(page.Items, event)
			}
			if len(page.Items) >= limit {
				break
			}
		}
		if cursor.BatchOffset == len(batch.Events) {
			cursor.Storage, cursor.BatchKey, cursor.BatchNext, cursor.BatchOffset = cursor.BatchNext, "", "", 0
			if cursor.Storage == "" {
				cursor.Phase = 2
			}
		}
		if len(page.Items) >= limit {
			break
		}
	}
	return nil
}

func diagnosticEventMatches(event diagnostics.Event, values url.Values, since, before, historyBefore time.Time) bool {
	if !since.IsZero() && event.OccurredAt.Before(since) || !before.IsZero() && !event.OccurredAt.Before(before) || event.OccurredAt.Before(historyBefore) {
		return false
	}
	identity := inventory.Identity{Authority: event.Authority, Repository: event.Repository}
	imageID := event.ImageID
	if imageID == "" {
		imageID = identity.ContentID(event.Digest)
	}
	fields := map[string]string{"component": event.Component, "operation": event.Operation, "imageID": imageID, "authority": event.Authority, "repository": event.Repository, "digest": event.Digest, "taskID": event.TaskID, "intentID": event.IntentID, "transferID": event.TransferID, "workID": event.WorkID, "runID": event.RunID, "attemptID": event.AttemptID, "parentAttemptID": event.ParentAttemptID, "attemptLayer": event.AttemptLayer, "result": event.Result, "errorClass": event.ErrorClass, "timeoutScope": event.TimeoutScope}
	for key, value := range fields {
		if values.Get(key) != "" && values.Get(key) != value {
			return false
		}
	}
	return true
}
