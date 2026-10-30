package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"slices"
	"strings"
	"time"
)

type backgroundJobPage = diagnosticPage[backgroundJobView]

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
	q := workqueue.Queue{Store: s.store}
	if len(parts) == 2 && request.Method == http.MethodGet && len(request.URL.Query()) == 0 {
		job, err := q.Read(ctx, parts[0], parts[1])
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, diagnosticJob(job))
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
	if (values.Get("state") == "done" || values.Get("state") == "superseded") && cursor.Phase == 0 {
		cursor.Phase = 1
		cursor.Storage = ""
	}
	prefix := workqueue.Prefix + "jobs/"
	if cursor.Phase == 1 {
		prefix = workqueue.Prefix + "receipts/"
	}
	if kind != "" {
		prefix += kind + "/"
	}
	entries, next, err := storage.Scan(ctx, s.store, prefix, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := backgroundJobPage{Items: []backgroundJobView{}, ObservedAt: time.Now().UTC(), Consistency: "live; deduplicate by kind/id/revision", Scanned: len(entries)}
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
		if cursor.Phase == 0 && job.State == "done" {
			continue
		}
		view := diagnosticJob(job)
		if matchesWork(view, values) {
			page.Items = append(page.Items, view)
		}
	}
	cursor.Storage = next
	if next == "" && cursor.Phase == 0 {
		cursor.Phase = 1
		next = "receipts"
	}
	if next != "" {
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}

func (s *Service) retryBackground(writer http.ResponseWriter, request *http.Request, ctx context.Context, kind, id string) {
	q := workqueue.Queue{Store: s.store, ValidateRetrySource: s.validateRetrySource}
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
