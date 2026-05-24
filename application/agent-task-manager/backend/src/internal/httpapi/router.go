package httpapi

import (
	"net/http"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/service"
)

type Server struct {
	service      *service.Service
	artifactsDir string
	authMode     string
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type artifactUploadResponse struct {
	ArtifactName string `json:"artifact_name"`
	ArtifactPath string `json:"artifact_path"`
}

func New(service *service.Service, artifactsDir, authMode string) *Server {
	return &Server{service: service, artifactsDir: artifactsDir, authMode: authMode}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/api/v1/auth/me", s.wrapAuth(s.handleAuthMe))
	mux.HandleFunc("/api/v1/users", s.wrapAuth(s.handleUsers))
	mux.HandleFunc("/api/v1/users/", s.wrapAuth(s.handleUserSubresources))
	mux.HandleFunc("/api/v1/projects", s.wrapAuth(s.handleProjects))
	mux.HandleFunc("/api/v1/projects/", s.wrapAuth(s.handleProjectSubresources))
	mux.HandleFunc("/api/v1/tasks", s.wrapAuth(s.handleTasks))
	mux.HandleFunc("/api/v1/tasks/", s.wrapAuth(s.handleTaskSubresources))
	mux.HandleFunc("/api/v1/comments/", s.wrapAuth(s.handleCommentSubresources))
	mux.HandleFunc("/api/v1/sessions", s.wrapAuth(s.handleSessions))
	mux.HandleFunc("/api/v1/sessions/uploads", s.wrapAuth(s.handleSessionUpload))
	mux.HandleFunc("/api/v1/sessions/", s.wrapAuth(s.handleSessionSubresources))
	mux.HandleFunc("/api/v1/activities", s.wrapAuth(s.handleActivities))
	mux.HandleFunc("/api/v1/activities/", s.wrapAuth(s.handleActivitySubresources))
	mux.HandleFunc("/api/v1/search", s.wrapAuth(s.handleSearch))
	return mux
}
