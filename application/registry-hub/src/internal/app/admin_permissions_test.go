package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
)

func TestAdminRolesAreIsolated(t *testing.T) {
	permissionRoutes := []struct {
		method string
		path   string
		role   string
	}{
		{method: http.MethodGet, path: "/api/v1/tasks", role: auth.TaskAdmin},
		{method: http.MethodPost, path: "/api/v1/gc", role: auth.GCAdmin},
		{method: http.MethodPost, path: "/api/v1/referrers/reconcile", role: auth.ReferrerPrune},
	}
	for _, route := range permissionRoutes {
		if got := adminPermission(route.method, route.path); got != route.role {
			t.Errorf("adminPermission(%s, %s) = %q, want %q", route.method, route.path, got, route.role)
		}
	}

	for _, principalRole := range permissionRoutes {
		for _, target := range permissionRoutes {
			if principalRole.role == target.role {
				continue
			}
			t.Run(principalRole.role+" cannot access "+target.role, func(t *testing.T) {
				service := &Service{cfg: config.Config{AdminAuth: config.Auth{Basic: []config.Credential{{
					Username: "operator", Password: "secret", Permissions: []string{principalRole.role},
				}}}}}
				request := httptest.NewRequest(target.method, target.path, nil)
				request.SetBasicAuth("operator", "secret")
				response := httptest.NewRecorder()
				service.handleAdmin(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatalf("admin request status = %d, want %d: %s", response.Code, http.StatusForbidden, response.Body.String())
				}
			})
		}
	}
}
