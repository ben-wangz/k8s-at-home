package app

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (s *Service) serveHistoryCleanup(writer http.ResponseWriter, request *http.Request, ctx context.Context, principal auth.Principal) {
	writer.Header().Set("Cache-Control", "no-store")
	switch {
	case request.URL.Path == "/api/v1/history/retention" && request.Method == http.MethodGet:
		if len(request.URL.Query()) != 0 {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		view, err := s.historyCleanupRetention(ctx)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, view)
	case request.URL.Path == "/api/v1/history/cleanup/preview" && request.Method == http.MethodPost:
		var body historyCleanupPreviewRequest
		if len(request.URL.Query()) != 0 || decodeAdminBody(writer, request, &body) != nil || !historyCleanupUTC(body.Cutoff) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		preview, err := s.createHistoryCleanupPreview(ctx, principal.Name, body)
		if errors.Is(err, errHistoryCleanupCapacity) {
			writeAdminError(writer, http.StatusTooManyRequests, "CAPACITY", "cleanup preview capacity is full")
			return
		}
		if errors.Is(err, storage.ErrCorruptObject) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, preview)
	case request.URL.Path == "/api/v1/history/cleanup" && request.Method == http.MethodPost:
		var body historyCleanupExecutionRequest
		if len(request.URL.Query()) != 0 || decodeAdminBody(writer, request, &body) != nil {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		view, status, err := s.submitHistoryCleanup(ctx, principal.Name, request.Header.Get("Idempotency-Key"), body)
		if err != nil {
			s.writeHistoryCleanupError(writer, status, err)
			return
		}
		writeJSON(writer, http.StatusAccepted, view)
	case len(request.URL.Query()) == 0 && request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/v1/history/cleanup/"):
		id := strings.TrimPrefix(request.URL.Path, "/api/v1/history/cleanup/")
		if !inventory.ValidID(id) {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		view, err := s.historyCleanupStatus(ctx, id, principal.Name)
		if errors.Is(err, inventory.ErrExpired) {
			writeAdminError(writer, http.StatusGone, "HISTORY_EXPIRED", "cleanup result is outside the query window")
			return
		}
		if errors.Is(err, storage.ErrNotFound) {
			writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "cleanup operation does not exist")
			return
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, view)
	default:
		writeAdminError(writer, http.StatusNotFound, "UNSUPPORTED", "history cleanup endpoint does not exist")
	}
}

func (s *Service) writeHistoryCleanupError(writer http.ResponseWriter, status int, err error) {
	switch status {
	case http.StatusBadRequest:
		writeInventoryError(writer, inventory.ErrInvalid)
	case http.StatusNotFound:
		writeAdminError(writer, http.StatusNotFound, "NAME_UNKNOWN", "cleanup preview or operation does not exist")
	case http.StatusGone:
		writeAdminError(writer, http.StatusGone, "HISTORY_EXPIRED", "cleanup preview has expired")
	case http.StatusConflict:
		writeAdminError(writer, http.StatusConflict, "REVISION_CONFLICT", "cleanup preview, policy, or idempotency input conflicts")
	case http.StatusTooManyRequests:
		writeAdminError(writer, http.StatusTooManyRequests, "CAPACITY", "cleanup operation capacity is full")
	default:
		writeAdminError(writer, http.StatusServiceUnavailable, "UNAVAILABLE", "cleanup request could not be persisted")
	}
}

func historyCleanupUTC(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	_, offset := value.Zone()
	return offset == 0
}
