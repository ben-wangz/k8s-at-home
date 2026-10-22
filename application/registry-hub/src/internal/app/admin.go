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
		query := request.URL.Query()
		if !onlyQueryKeys(query, "limit", "token", "ownersFor", "ownerLimit", "ownerToken") {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection query parameters are invalid")
			return
		}
		if query.Get("token") != "" && !validGateID(query.Get("token")) {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection page token is invalid")
			return
		}
		limit := 100
		if raw := query.Get("limit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection page limit is invalid")
				return
			}
			limit = parsed
		}
		ownersFor := query.Get("ownersFor")
		ownerToken := query.Get("ownerToken")
		ownerLimit := 100
		if ownersFor != "" && !validGateID(ownersFor) || ownersFor == "" && (ownerToken != "" || query.Get("ownerLimit") != "") {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection owner query is invalid")
			return
		}
		if raw := query.Get("ownerLimit"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection owner page limit is invalid")
				return
			}
			ownerLimit = parsed
		}
		if !validAdminPageToken(ownerToken, 4096) {
			writeAdminError(writer, http.StatusBadRequest, "UNSUPPORTED", "garbage collection owner page token is invalid")
			return
		}
		status, err := s.gcStatus(ctx, query.Get("token"), limit, ownersFor, ownerToken, ownerLimit)
		if err != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "garbage collection state could not be read")
			return
		}
		writeJSON(writer, http.StatusOK, status)
	case request.URL.Path == "/api/v1/gc" && request.Method == http.MethodPost:
		run, err := s.gc.Trigger(false)
		if err != nil {
			s.writeGCConflict(writer, ctx, err, "GC could not be started")
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
			s.writeGCConflict(writer, ctx, err, "GC retry was refused")
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
			s.writeGCConflict(writer, ctx, err, "GC recovery was refused")
			return
		}
		status, statusErr := s.gcStatus(ctx, "", 100, "", "", 100)
		if statusErr != nil {
			writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "garbage collection state could not be read")
			return
		}
		status.Run = run
		writeJSON(writer, http.StatusOK, status)
	case request.URL.Path == "/api/v1/referrers/reconcile":
		s.reconcileReferrers(writer, request, ctx)
	default:
		registryapi.WriteError(writer, http.StatusMethodNotAllowed, "UNSUPPORTED", "management method is not supported")
	}
}

func (s *Service) gcStatus(ctx context.Context, token string, limit int, ownersFor, ownerToken string, ownerLimit int) (adminGCStatusResponse, error) {
	sessions, next, err := s.gate.ListProtectionSessions(ctx, token, limit)
	if err != nil {
		return adminGCStatusResponse{}, err
	}
	status := adminGCStatusResponse{Gate: s.gate.State(), Run: s.gc.Snapshot(), Sessions: sessions, NextToken: next}
	if ownersFor != "" {
		owners, ownersNext, err := s.gate.ListProtectionSessionOwners(ctx, ownersFor, ownerToken, ownerLimit)
		if err != nil {
			return adminGCStatusResponse{}, err
		}
		status.OwnerSessionID = ownersFor
		status.Owners = owners
		status.OwnersNextToken = ownersNext
	}
	return status, nil
}

func (s *Service) writeGCConflict(writer http.ResponseWriter, ctx context.Context, err error, fallback string) {
	if errors.Is(err, ErrGCProtected) || errors.Is(err, ErrRecoveryBarrier) {
		detail := adminGCConflictDetail{Unavailable: true}
		if status, statusErr := s.gcStatus(ctx, "", 100, "", "", 100); statusErr == nil {
			detail = adminGCConflictDetail{Gate: &status.Gate, Blockers: status.Sessions, NextToken: status.NextToken}
		}
		registryapi.WriteErrorWithDetail(writer, http.StatusConflict, "GC_BLOCKED", "GC is blocked by active solidification protection or recovery", detail)
		return
	}
	writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", fallback)
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
	filtered := make([]adminTaskView, 0, len(items))
	for _, item := range items {
		if query.Get("state") != "" && string(item.Status) != query.Get("state") || query.Get("authority") != "" && item.Authority != query.Get("authority") || query.Get("repository") != "" && item.Repository != query.Get("repository") || query.Get("digest") != "" && item.RootDigest != query.Get("digest") && item.Reference != query.Get("digest") {
			continue
		}
		if !since.IsZero() && item.CreatedAt.Before(since) || !before.IsZero() && !item.CreatedAt.Before(before) {
			continue
		}
		view, err := s.taskAdminView(ctx, item)
		if err != nil {
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
	protected := task.GCProtected || len(task.SessionIDs) > 0 || task.ProtectionSessionID != "" || task.RecoveryReason != ""
	return adminTaskView{Task: task, GCProtectionActive: protected}
}

func writeTaskError(writer http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "task does not exist")
		return
	}
	writeAdminError(writer, http.StatusConflict, "UNAVAILABLE", "task operation was refused")
}
