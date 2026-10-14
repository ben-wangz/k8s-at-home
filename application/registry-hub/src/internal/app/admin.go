package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/registryapi"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strconv"
	"strings"
	"time"
)

func (s *Service) handleAdmin(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.URL.Path == "/readyz" {
		if !s.ready.Load() {
			http.Error(writer, "not ready", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		return
	}
	permission := adminPermission(request.Method, request.URL.Path)
	if permission == "" {
		registryapi.WriteError(writer, http.StatusNotFound, "UNSUPPORTED", "management endpoint does not exist")
		return
	}
	principal, ok := auth.Authenticate(request, s.cfg.AdminAuth, false)
	if !ok {
		auth.Challenge(writer, "registry-hub-admin")
		registryapi.WriteError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "management authentication is required")
		return
	}
	if !principal.Allows(permission) {
		registryapi.WriteError(writer, http.StatusForbidden, "DENIED", "management permission is required")
		return
	}
	if request.URL.Path == "/metrics" {
		s.metrics.Handler().ServeHTTP(writer, request)
		return
	}
	adminTimeout := 30 * time.Second
	if request.URL.Path == "/api/v1/referrers/reconcile" && request.Method == http.MethodPost {
		adminTimeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(request.Context(), adminTimeout)
	defer cancel()
	switch {
	case request.URL.Path == "/api/v1/tasks" && request.Method == http.MethodGet:
		s.listTasks(writer, request, ctx)
	case request.URL.Path == "/api/v1/tasks/cleanup" && request.Method == http.MethodPost:
		uploadCount, err := s.uploads.CleanupExpired(ctx, s.gate, 1000)
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "upload session cleanup failed")
			return
		}
		cacheCount, err := s.cache.Cleanup(ctx, time.Now().UTC(), 1000)
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "cache cleanup failed")
			return
		}
		orphanCacheCount, err := s.cache.CleanupOrphans(ctx, time.Now().UTC(), time.Hour, 1000)
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "cache orphan cleanup failed")
			return
		}
		storageOrphanCount := 0
		if cleaner, ok := s.store.(interface {
			CleanupOrphans(context.Context, time.Duration, int) (int, error)
		}); ok {
			storageOrphanCount, err = cleaner.CleanupOrphans(ctx, time.Hour, 1000)
			if err != nil {
				writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "storage orphan cleanup failed")
				return
			}
		}
		count, err := s.tasks.Cleanup(ctx, 30*24*time.Hour, 1000)
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "task cleanup failed")
			return
		}
		writeJSON(writer, http.StatusOK, adminTaskCleanupResponse{
			Removed: count, UploadSessionsRemoved: uploadCount, CacheRemoved: cacheCount,
			OrphanCacheRemoved: orphanCacheCount, StorageOrphanRemoved: storageOrphanCount,
		})
	case strings.HasPrefix(request.URL.Path, "/api/v1/tasks/"):
		s.taskOperation(writer, request, ctx)
	case request.URL.Path == "/api/v1/gc" && request.Method == http.MethodGet:
		writeJSON(writer, http.StatusOK, adminGCStatusResponse{Gate: s.gate.State(), Run: s.gc.Snapshot()})
	case request.URL.Path == "/api/v1/gc" && request.Method == http.MethodPost:
		run, err := s.gc.Trigger(false)
		if err != nil {
			writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "GC could not be started")
			return
		}
		writeJSON(writer, http.StatusAccepted, run)
	case request.URL.Path == "/api/v1/gc/retry" && request.Method == http.MethodPost:
		var input adminGCConfirmationRequest
		if err := decodeAdminBody(writer, request, &input); err != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "retry confirmation is invalid")
			return
		}
		run, err := s.gc.Retry(ctx, input.ConfirmedNoExecutor)
		if err != nil {
			writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "GC retry was refused")
			return
		}
		writeJSON(writer, http.StatusAccepted, run)
	case request.URL.Path == "/api/v1/gc/recover" && request.Method == http.MethodPost:
		var input adminGCConfirmationRequest
		if err := decodeAdminBody(writer, request, &input); err != nil {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "recovery confirmation is invalid")
			return
		}
		run, err := s.gc.Recover(ctx, input.ConfirmedNoExecutor)
		if err != nil {
			writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "GC recovery was refused")
			return
		}
		writeJSON(writer, http.StatusOK, adminGCStatusResponse{Gate: s.gate.State(), Run: run})
	case request.URL.Path == "/api/v1/referrers/reconcile":
		s.reconcileReferrers(writer, request, ctx)
	default:
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "management method is not supported")
	}
}

func adminPermission(method, path string) string {
	if path == "/metrics" {
		return auth.TaskAdmin
	}
	if strings.HasPrefix(path, "/api/v1/gc") {
		if method == http.MethodGet || method == http.MethodPost {
			return auth.GCAdmin
		}
		return ""
	}
	if strings.HasPrefix(path, "/api/v1/referrers/reconcile") {
		if method == http.MethodGet || method == http.MethodPost {
			return auth.ReferrerPrune
		}
		return ""
	}
	if path == "/api/v1/tasks" || strings.HasPrefix(path, "/api/v1/tasks/") {
		if method == http.MethodGet || method == http.MethodPost {
			return auth.TaskAdmin
		}
	}
	return ""
}

func (s *Service) listTasks(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil || !onlyQueryKeys(query, "state", "authority", "repository", "digest", "since", "before", "limit", "token") {
		writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "task query is invalid")
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
	filtered := make([]tasks.Task, 0, len(items))
	for _, item := range items {
		if query.Get("state") != "" && string(item.Status) != query.Get("state") || query.Get("authority") != "" && item.Authority != query.Get("authority") || query.Get("repository") != "" && item.Repository != query.Get("repository") || query.Get("digest") != "" && item.RootDigest != query.Get("digest") && item.Reference != query.Get("digest") {
			continue
		}
		if !since.IsZero() && item.CreatedAt.Before(since) || !before.IsZero() && !item.CreatedAt.Before(before) {
			continue
		}
		filtered = append(filtered, item)
	}
	writeJSON(writer, http.StatusOK, adminTaskListResponse{Items: filtered, NextToken: next})
}

func (s *Service) taskOperation(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/v1/tasks/"), "/")
	if len(parts) == 1 && request.Method == http.MethodGet {
		task, err := s.tasks.Get(ctx, parts[0])
		if err != nil {
			writeTaskError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, task)
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
		task, err = s.tasks.Retry(ctx, parts[0])
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
	writeJSON(writer, http.StatusAccepted, task)
}

func writeTaskError(writer http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "task does not exist")
		return
	}
	writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "task operation was refused")
}
