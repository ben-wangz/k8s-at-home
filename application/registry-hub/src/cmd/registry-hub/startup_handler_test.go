package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartupHealthRemainsAvailableBeforeServiceInitialization(t *testing.T) {
	for _, admin := range []bool{false, true} {
		handler := &startupHandler{admin: admin}
		for _, path := range []string{"/healthz", "/readyz", "/v2/", "/api/v1/workers"} {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			expected := http.StatusServiceUnavailable
			if path == "/healthz" {
				expected = http.StatusOK
			}
			if response.Code != expected {
				t.Fatalf("admin=%t path=%s returned %d, expected %d", admin, path, response.Code, expected)
			}
		}
	}
}
