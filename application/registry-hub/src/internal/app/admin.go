package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/auth"
	"registry-hub/internal/registryapi"
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
	readObservation := request.Method == http.MethodGet && (request.URL.Path == "/api/v1/gc" || request.URL.Path == "/api/v1/referrers/reconcile") && principal.Allows(auth.Observe)
	if !principal.Allows(permission) && !readObservation {
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
	if request.Method == http.MethodGet {
		ctx = context.WithValue(ctx, diagnosticReadKey{}, true)
	}
	switch {
	case diagnosticAdminPath(request.URL.Path):
		s.serveDiagnostics(writer, request, ctx)
	case request.URL.Path == "/api/v1/background" || strings.HasPrefix(request.URL.Path, "/api/v1/background/"):
		s.serveBackground(writer, request, ctx)
	case request.Method == http.MethodGet && inventoryAdminPath(request.URL.Path):
		s.serveInventory(writer, request, ctx)
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
	case request.URL.Path == "/api/v1/gc/reconcile" && request.Method == http.MethodPost:
		s.reconcileGCNoRun(writer, request, ctx, principal)
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
