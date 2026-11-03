package main

import (
	"net/http"
	"registry-hub/internal/app"
	"sync/atomic"
)

type startupHandler struct {
	service atomic.Pointer[app.Service]
	admin   bool
}

func (h *startupHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if service := h.service.Load(); service != nil {
		handler := service.ClientHandler()
		if h.admin {
			handler = service.AdminHandler()
		}
		handler.ServeHTTP(writer, request)
		return
	}
	if request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	writer.Header().Set("Retry-After", "1")
	http.Error(writer, "initialization is in progress", http.StatusServiceUnavailable)
}
