package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"time"
)

func (s *Service) listTasks(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !onlyQueryKeys(query, "state", "authority", "repository", "digest", "since", "before", "limit", "token", "kind", "waitReason", "errorClass", "retryable", "imageID", "blobDigest") {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task query is invalid")
		return
	}
	if v := query.Get("retryable"); v != "" && v != "true" && v != "false" {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "retryable must be true or false")
		return
	}
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 1000 {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task page size is invalid")
			return
		}
	}
	var since, before time.Time
	if raw := query.Get("since"); raw != "" {
		since, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task time filter is invalid")
			return
		}
	}
	if raw := query.Get("before"); raw != "" {
		before, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task time filter is invalid")
			return
		}
	}
	items, next, err := s.tasks.List(ctx, "", query.Get("token"), limit)
	if err != nil {
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task state is unavailable")
		return
	}
	filtered := make([]adminTaskView, 0, len(items))
	for _, item := range items {
		if !matchesTaskDiagnostics(item, query) {
			continue
		}
		if query.Get("state") != "" && string(item.Status) != query.Get("state") || query.Get("authority") != "" && item.Authority != query.Get("authority") || query.Get("repository") != "" && item.Repository != query.Get("repository") || query.Get("digest") != "" && item.RootDigest != query.Get("digest") && item.Reference != query.Get("digest") && item.BlobDigest != query.Get("digest") {
			continue
		}
		if !since.IsZero() && item.CreatedAt.Before(since) || !before.IsZero() && !item.CreatedAt.Before(before) {
			continue
		}
		view, err := s.taskAdminView(ctx, item)
		if err != nil {
			s.logAdminTaskViewFailure(item.ID, err)
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task protection state is unavailable")
			return
		}
		filtered = append(filtered, view)
	}
	writeJSON(writer, http.StatusOK, adminTaskListResponse{Items: filtered, NextToken: next})
}

func (s *Service) taskOperation(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/tasks/"), "/")
	if len(parts) == 1 && request.Method == http.MethodGet {
		options, optionErr := parseAdminTaskDetailOptions(request.URL.Query())
		if optionErr != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task detail query is invalid")
			return
		}
		task, err := s.tasks.Get(ctx, parts[0])
		if err != nil {
			writeTaskError(writer, err)
			return
		}
		view, err := s.taskDetailView(ctx, task, options)
		if err != nil {
			if errors.Is(err, errAdminTaskOwnerSession) {
				writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "owner session does not belong to task")
				return
			}
			s.logAdminTaskViewFailure(parts[0], err)
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task detail could not be read")
			return
		}
		writeJSON(writer, http.StatusOK, view)
		return
	}
	if len(parts) != 2 || request.Method != http.MethodPost {
		registryapi.WriteError(writer, http.StatusNotFound, "UNSUPPORTED", "task endpoint does not exist")
		return
	}
	var task tasks.Task
	var err error
	switch parts[1] {
	case "retry":
		task, err = s.retryTask(ctx, parts[0])
	case "cancel":
		task, err = s.tasks.Cancel(ctx, parts[0])
	default:
		registryapi.WriteError(writer, http.StatusNotFound, "UNSUPPORTED", "task operation does not exist")
		return
	}
	if err != nil {
		writeTaskError(writer, err)
		return
	}
	status := http.StatusAccepted
	if task.TerminalCleanupComplete && !task.GCProtected && len(task.SessionIDs) == 0 && task.ProtectionSessionID == "" && task.RecoveryReason == "" {
		status = http.StatusOK
	}
	view, viewErr := s.taskAdminView(ctx, task)
	if viewErr != nil {
		s.logAdminTaskViewFailure(task.ID, viewErr)
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task protection state is unavailable")
		return
	}
	writeJSON(writer, status, view)
}

func (s *Service) retryTask(ctx context.Context, id string) (tasks.Task, error) {
	task, err := s.tasks.Get(ctx, id)
	if err != nil {
		return tasks.Task{}, err
	}
	if task.Kind != tasks.BlobKind || task.Status != tasks.Failed || task.ErrorClass != "capacity_limit" {
		return s.tasks.Retry(ctx, id)
	}
	if task.CleanupStarted {
		return tasks.Task{}, errors.New("task history cleanup has started")
	}
	intent, err := s.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		return tasks.Task{}, err
	}
	cached, err := s.cache.GetAnyRetained(ctx, intent.Identity.Authority, intent.Identity.Repository, intent.Identity.Digest)
	if err != nil {
		return tasks.Task{}, err
	}
	defer cached.Body.Close()
	nextIntent, err := s.records.StartBlobIntentRound(ctx, intent.ID)
	if err != nil {
		return tasks.Task{}, err
	}
	ready, err := s.records.SetBlobIntentReady(ctx, nextIntent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		return tasks.Task{}, err
	}
	if ready.State == tasks.IntentAnchored || ready.State == tasks.IntentVerified {
		return s.tasks.Get(ctx, ready.ID)
	}
	if ready.State != tasks.IntentReady && ready.State != tasks.IntentRetryWait {
		return tasks.Task{}, errors.New("new blob intent round is not ready")
	}
	newTask, _, err := s.enqueueBlobIntent(ctx, ready)
	if err != nil {
		return tasks.Task{}, err
	}
	s.wakeBlobDemandRoots(ctx, ready.ID)
	return newTask, nil
}

func taskAdminView(task tasks.Task) adminTaskView {
	task.DiagnosticOutbox = nil
	protected := task.GCProtected || len(task.SessionIDs) > 0 || task.ProtectionSessionID != ""
	return adminTaskView{Task: task, GCProtectionActive: protected}
}

func writeTaskError(writer http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "task does not exist")
		return
	}
	writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "task operation was refused")
}
