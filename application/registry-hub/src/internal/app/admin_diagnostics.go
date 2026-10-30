package app

import (
	"context"
	"net/http"
	"registry-hub/internal/inventory"
	"strings"
)

func diagnosticAdminPath(path string) bool {
	if path == "/api/v1/download-reservations" {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/images/") && (strings.HasSuffix(path, "/projection") || strings.HasSuffix(path, "/dependencies") || strings.HasSuffix(path, "/execution-facts")) {
		return true
	}
	if path == "/api/v1/transfers" || strings.HasPrefix(path, "/api/v1/transfers/") {
		return true
	}
	if strings.HasPrefix(path, "/api/v1/storage/") {
		return true
	}
	if path == "/api/v1/events" {
		return true
	}
	return path == "/api/v1/status" || path == "/api/v1/workers" || strings.HasPrefix(path, "/api/v1/workers/") || path == "/api/v1/blob-intents" || strings.HasPrefix(path, "/api/v1/blob-intents/") || strings.HasPrefix(path, "/api/v1/tasks/") && strings.HasSuffix(path, "/dependencies")
}

func (s *Service) serveDiagnostics(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	if request.Method != http.MethodGet {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	switch {
	case strings.HasPrefix(request.URL.Path, "/api/v1/images/") && strings.HasSuffix(request.URL.Path, "/execution-facts"):
		s.serveExecutionFacts(writer, request, ctx)
	case request.URL.Path == "/api/v1/download-reservations":
		s.serveReservations(writer, request, ctx)
	case strings.HasPrefix(request.URL.Path, "/api/v1/images/") && strings.HasSuffix(request.URL.Path, "/projection"):
		s.serveImageProjection(writer, request, ctx)
	case strings.HasPrefix(request.URL.Path, "/api/v1/images/") && strings.HasSuffix(request.URL.Path, "/dependencies"):
		s.serveImageDependencies(writer, request, ctx)
	case strings.HasPrefix(request.URL.Path, "/api/v1/transfers"):
		s.serveTransfers(writer, request, ctx)
	case strings.HasPrefix(request.URL.Path, "/api/v1/storage/"):
		s.serveStorage(writer, request, ctx)
	case request.URL.Path == "/api/v1/events":
		s.serveEvents(writer, request, ctx)
	case request.URL.Path == "/api/v1/status":
		if len(request.URL.Query()) != 0 {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
		status, err := s.systemStatus(ctx)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, status)
	case strings.HasPrefix(request.URL.Path, "/api/v1/workers"):
		s.serveWorkers(writer, request, ctx)
	case strings.HasPrefix(request.URL.Path, "/api/v1/blob-intents"):
		if strings.Contains(request.URL.Path, "/notifications") {
			s.serveIntentNotifications(writer, request, ctx)
		} else {
			s.serveBlobIntents(writer, request, ctx)
		}
	case strings.HasSuffix(request.URL.Path, "/dependencies"):
		s.serveTaskDependencies(writer, request, ctx)
	}
}
