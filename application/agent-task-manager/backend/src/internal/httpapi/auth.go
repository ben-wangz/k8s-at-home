package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store/bunstore"
)

var errMissingBearerToken = errors.New("missing bearer token")
var errInvalidAPIKey = errors.New("invalid api key")

func (s *Server) wrapAuth(next func(http.ResponseWriter, *http.Request, *domain.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Agent-Task-Manager-Auth-Mode", s.authMode)
		user, err := s.currentUser(r.Context(), r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		next(w, r, user)
	}
}

func (s *Server) currentUser(ctx context.Context, r *http.Request) (*domain.User, error) {
	if s.authMode != "apikey" {
		user := &domain.User{ID: "system", Email: "system@local", Name: "system", Role: "admin", Active: true}
		return user, nil
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return nil, errMissingBearerToken
	}
	token := strings.TrimSpace(authorization[7:])
	if token == "" {
		return nil, errMissingBearerToken
	}
	user, err := s.service.Store().GetUserByAPIKey(ctx, token)
	if err != nil {
		if errors.Is(err, bunstore.ErrNotFound) {
			return nil, errInvalidAPIKey
		}
		return nil, err
	}
	if !user.Active {
		return nil, errors.New("api key user is inactive")
	}
	return &user, nil
}

func (s *Server) handleAuthMe(w http.ResponseWriter, _ *http.Request, user *domain.User) {
	writeJSON(w, http.StatusOK, user)
}
