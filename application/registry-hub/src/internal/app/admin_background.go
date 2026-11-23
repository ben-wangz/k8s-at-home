package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"strings"
	"time"
)

type backgroundJobPage struct {
	diagnosticPage[backgroundJobView]
	HistoryBefore time.Time `json:"historyBefore"`
	Coverage      string    `json:"coverage"`
}

var workQueryFields = []string{"kind", "state", "reason", "authority", "repository", "imageID", "taskID", "intentID", "transferID", "digest", "errorClass", "retryable"}

func (s *Service) serveBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/background"), "/")
	if len(parts) > 0 && parts[0] == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && !slices.Contains(backgroundKinds, parts[0]) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	if len(parts) <= 1 && request.Method == http.MethodGet {
		kind := ""
		if len(parts) == 1 {
			kind = parts[0]
		}
		s.listBackground(writer, request, ctx, kind)
		return
	}
	if len(parts) < 2 || !inventory.ValidID(parts[1]) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if len(parts) == 2 && request.Method == http.MethodGet && len(request.URL.Query()) == 0 {
		job, version, err := inventory.Read[workqueue.Job](ctx, s.store, workqueue.Key(parts[0], parts[1]))
		if err == nil {
			job.Version = version
			writeJSON(writer, http.StatusOK, diagnosticJob(job))
			return
		}
		if !errors.Is(err, storage.ErrNotFound) {
			writeInventoryError(writer, err)
			return
		}
		view, historyErr := s.terminalWorkHistoryDetail(ctx, parts[0], parts[1])
		if historyErr == nil {
			writeJSON(writer, http.StatusOK, view)
			return
		}
		if errors.Is(historyErr, inventory.ErrExpired) {
			writeAdminError(writer, http.StatusGone, "HISTORY_EXPIRED", "terminal work history is outside the configured retention window")
			return
		}
		writeInventoryError(writer, historyErr)
		return
	}
	if len(parts) == 3 && parts[2] == "retry" && request.Method == http.MethodPost && len(request.URL.Query()) == 0 {
		s.retryBackground(writer, request, ctx, parts[0], parts[1])
		return
	}
	writeInventoryError(writer, inventory.ErrInvalid)
}

func (s *Service) listBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context, kind string) {
	values, cursor, limit, err := s.diagnosticQuery(request, workQueryFields...)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	if state := values.Get("state"); state != "" && !slices.Contains([]string{"pending", "running", "waiting_dependency", "retry_wait", "failed", "done", "superseded"}, state) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if retryable := values.Get("retryable"); retryable != "" && retryable != "true" && retryable != "false" {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if filter := values.Get("kind"); filter != "" {
		if !slices.Contains(backgroundKinds, filter) || kind != "" && kind != filter {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		kind = filter
	}
	if kind != "" && values.Get("kind") == "" {
		values.Set("kind", kind)
	}
	if cursor.Phase < 0 || cursor.Phase > 1 {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if cursor.HistorySource == "" {
		if values.Get("token") != "" {
			cursor.HistorySource = terminalWorkHistoryLegacySource
		} else {
			cursor.HistorySource = terminalWorkHistorySource(s.cfg.Diagnostics.HistoryProjectionReadsEnabled)
		}
	}
	if cursor.HistoryBefore.IsZero() {
		cursor.HistoryBefore = time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	if cursor.Phase == 1 {
		s.listTerminalBackground(writer, ctx, values, cursor, limit)
		return
	}
	prefix := workqueue.Prefix + "jobs/"
	if kind != "" {
		prefix += kind + "/"
	}
	entries, next, err := storage.Scan(ctx, s.store, prefix, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := backgroundJobPage{diagnosticPage: diagnosticPage[backgroundJobView]{Items: []backgroundJobView{}, ObservedAt: time.Now().UTC(), Consistency: "active work and archived terminal history; history is ordered by completion time", Scanned: len(entries)}}
	for _, entry := range entries {
		job, version, readErr := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			writeInventoryError(writer, readErr)
			return
		}
		job.Version = version
		if job.State == "done" {
			terminalAt := job.CompletedAt
			if terminalAt.IsZero() {
				terminalAt = time.Unix(0, entry.ModTime).UTC()
			}
			if terminalAt.IsZero() || !terminalAt.After(cursor.HistoryBefore) {
				continue
			}
		}
		view := diagnosticJob(job)
		if matchesWork(view, values) {
			page.Items = append(page.Items, view)
		}
	}
	cursor.Storage = next
	if next == "" && cursor.Phase == 0 {
		cursor.Phase, cursor.Storage = 1, ""
		page.HistoryBefore = cursor.HistoryBefore
	}
	if next != "" {
		page.NextToken = s.diagnosticToken(cursor)
		writeJSON(writer, http.StatusOK, page)
		return
	}
	if cursor.Phase == 1 {
		if len(page.Items) >= limit {
			page.NextToken = s.diagnosticToken(cursor)
		} else if err := s.appendTerminalWorkHistory(ctx, values, &cursor, limit-len(page.Items), &page); err != nil {
			writeInventoryError(writer, err)
			return
		}
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) listTerminalBackground(writer http.ResponseWriter, ctx context.Context, values url.Values, cursor diagnosticCursor, limit int) {
	if cursor.HistorySource == "" {
		if cursor.Storage != "" {
			writeInventoryError(writer, inventory.ErrExpired)
			return
		}
		cursor.HistorySource = terminalWorkHistorySource(s.cfg.Diagnostics.HistoryProjectionReadsEnabled)
		cursor.HistoryBefore = time.Now().UTC().Add(-s.diagnosticsConfig().TerminalHistoryRetention)
	}
	if cursor.HistorySource != terminalWorkHistoryLegacySource && cursor.HistorySource != terminalWorkHistoryIndexSource {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	page := backgroundJobPage{diagnosticPage: diagnosticPage[backgroundJobView]{Items: []backgroundJobView{}, ObservedAt: time.Now().UTC(), Consistency: "terminal work history ordered by completion time"}}
	page.HistoryBefore = cursor.HistoryBefore
	page.Coverage = "terminal work history retained for " + s.diagnosticsConfig().TerminalHistoryRetention.String() + "; completion markers preserve 410 expiry evidence"
	if cursor.HistorySource == terminalWorkHistoryLegacySource {
		page.Coverage += "; legacy receipt fallback remains enabled until projection backfill is verified"
	}
	if err := s.appendTerminalWorkHistory(ctx, values, &cursor, limit, &page); err != nil {
		writeInventoryError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) appendTerminalWorkHistory(ctx context.Context, values url.Values, cursor *diagnosticCursor, limit int, page *backgroundJobPage) error {
	rows, scanned, more, usedFallback, err := s.listTerminalWorkHistory(ctx, values, cursor, limit)
	if err != nil {
		return err
	}
	page.Scanned += scanned
	for _, record := range rows {
		page.Items = append(page.Items, record.View)
	}
	if usedFallback {
		page.Coverage += "; legacy receipts are included while projection backfill continues"
	}
	if more {
		if len(rows) > 0 && cursor.HistorySource == terminalWorkHistoryLegacySource {
			last := rows[len(rows)-1]
			cursor.HistoryAfter, cursor.HistoryKind, cursor.HistoryID, cursor.HistoryRevision = last.TerminalAt, last.Kind, last.ID, last.Revision
		}
		page.NextToken = s.diagnosticToken(*cursor)
	}
	return nil
}

func (s *Service) retryBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context, kind, id string) {
	q := s.backgroundQueue()
	q.ValidateRetrySource = s.validateRetrySource
	var result any
	var err error
	if request.Body != nil && request.ContentLength != 0 {
		var input workqueue.RetryRequest
		if decodeAdminBody(writer, request, &input) != nil {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		principal, _ := auth.Authenticate(request, s.cfg.AdminAuth, false)
		result, err = q.AcceptRetry(ctx, principal.Name, kind, id, input)
	} else {
		// Preserve the original task-admin endpoint; conditional requests provide durable idempotency.
		principal, _ := auth.Authenticate(request, s.cfg.AdminAuth, false)
		if !principal.Allows(auth.TaskAdmin) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		err = q.Retry(ctx, kind, id)
		if err == nil {
			var job workqueue.Job
			job, err = q.Read(ctx, kind, id)
			result = diagnosticJob(job)
		}
		writer.Header().Set("Warning", "299 registry-hub \"Use expectedRevision, sourceRevision and idempotencyKey\"")
	}
	if errors.Is(err, storage.ErrConditionFailed) {
		writeAdminError(writer, http.StatusConflict, "REVISION_CONFLICT", "work changed or cannot be retried")
		return
	}
	if errors.Is(err, workqueue.ErrSourceMissing) {
		writeAdminError(writer, http.StatusConflict, "SOURCE_MISSING", "the authoritative source required by this work is no longer available")
		return
	}
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	writer.Header().Set("Location", "/api/v1/background/"+kind+"/"+id)
	writeJSON(writer, http.StatusAccepted, result)
}
