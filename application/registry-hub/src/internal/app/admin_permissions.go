package app

import (
	"net/http"
	"registry-hub/internal/auth"
	"strings"
)

func adminPermission(method, path string) string {
	if method == http.MethodGet && (diagnosticAdminPath(path) || inventoryAdminPath(path) || strings.HasPrefix(path, "/api/v1/background") || path == "/api/v1/tasks" || strings.HasPrefix(path, "/api/v1/tasks/") || path == "/metrics") {
		return auth.Observe
	}
	if strings.HasPrefix(path, "/api/v1/background/") && method == http.MethodPost {
		return auth.WorkRetry
	}
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
