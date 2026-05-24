package httpapi

import (
	"net/http"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.service.Store().ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.User]{Items: users})
	case http.MethodPost:
		var input struct {
			Email  string `json:"email"`
			Name   string `json:"name"`
			Role   string `json:"role"`
			Active *bool  `json:"active"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		user, err := s.service.CreateUser(r.Context(), store.UserCreate{Email: input.Email, Name: input.Name, Role: input.Role, Active: input.Active == nil || *input.Active})
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, user)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleUserSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 {
		writeNotFound(w)
		return
	}
	userID := parts[3]
	if len(parts) == 4 {
		if r.Method != http.MethodPatch {
			writeMethodNotAllowed(w)
			return
		}
		var input struct {
			Email  *string `json:"email"`
			Name   *string `json:"name"`
			Role   *string `json:"role"`
			Active *bool   `json:"active"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		updated, err := s.service.UpdateUser(r.Context(), userID, store.UserUpdate{Email: input.Email, Name: input.Name, Role: input.Role, Active: input.Active})
		writeStoreResult(w, http.StatusOK, updated, err)
		return
	}
	if len(parts) == 5 && parts[4] == "api-keys" {
		s.handleUserAPIKeys(w, r, userID)
		return
	}
	if len(parts) == 6 && parts[4] == "api-keys" {
		if r.Method != http.MethodDelete {
			writeMethodNotAllowed(w)
			return
		}
		err := s.service.Store().DeleteAPIKey(r.Context(), userID, parts[5])
		writeDeleteResult(w, err)
		return
	}
	writeNotFound(w)
}

func (s *Server) handleUserAPIKeys(w http.ResponseWriter, r *http.Request, userID string) {
	switch r.Method {
	case http.MethodGet:
		keys, err := s.service.Store().ListAPIKeys(r.Context(), userID)
		if err != nil {
			writeStoreResult(w, http.StatusOK, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.APIKey]{Items: keys})
	case http.MethodPost:
		var input struct {
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		apiKey, err := s.service.CreateAPIKey(r.Context(), store.APIKeyCreate{UserID: userID, Name: input.Name})
		writeStoreResult(w, http.StatusCreated, apiKey, err)
	default:
		writeMethodNotAllowed(w)
	}
}
