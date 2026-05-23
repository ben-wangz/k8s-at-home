package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/service"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store/bunstore"
)

type Server struct {
	service      *service.Service
	artifactsDir string
	authMode     string
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
	mux.HandleFunc("/api/v1/tasks/", s.wrapAuth(s.handleTaskSubresources))
	mux.HandleFunc("/api/v1/comments/", s.wrapAuth(s.handleCommentSubresources))
	mux.HandleFunc("/api/v1/sessions", s.wrapAuth(s.handleSessions))
	mux.HandleFunc("/api/v1/sessions/uploads", s.wrapAuth(s.handleSessionUpload))
	mux.HandleFunc("/api/v1/sessions/", s.wrapAuth(s.handleSessionSubresources))
	mux.HandleFunc("/api/v1/activities", s.wrapAuth(s.handleActivities))
	return mux
}

func (s *Server) wrapAuth(next func(http.ResponseWriter, *http.Request, *domain.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := s.currentUser(r.Context(), r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
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
		return nil, errors.New("missing bearer token")
	}
	token := strings.TrimSpace(authorization[7:])
	user, err := s.service.Store().GetUserByAPIKey(ctx, token)
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.service.Store().Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleAuthMe(w http.ResponseWriter, _ *http.Request, user *domain.User) {
	writeJSON(w, http.StatusOK, user)
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		users, err := s.service.Store().ListUsers(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, users)
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
			writeError(w, http.StatusBadRequest, err.Error())
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
		writeStoreResult(w, http.StatusOK, keys, err)
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

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		projects, err := s.service.Store().ListProjects(r.Context())
		writeStoreResult(w, http.StatusOK, projects, err)
	case http.MethodPost:
		var input struct {
			Slug        string `json:"slug"`
			Title       string `json:"title"`
			Description string `json:"description"`
			State       string `json:"state"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		project, err := s.service.CreateProject(r.Context(), store.ProjectCreate{Slug: input.Slug, Title: input.Title, Description: input.Description, State: input.State})
		writeStoreResult(w, http.StatusCreated, project, err)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleProjectSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 {
		writeNotFound(w)
		return
	}
	projectID := parts[3]
	if len(parts) == 4 {
		s.handleProject(w, r, projectID)
		return
	}
	if len(parts) == 5 && parts[4] == "tasks" {
		s.handleProjectTasks(w, r, projectID)
		return
	}
	writeNotFound(w)
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		project, err := s.service.Store().GetProject(r.Context(), projectID)
		writeStoreResult(w, http.StatusOK, project, err)
	case http.MethodPatch:
		var input struct {
			Slug        *string `json:"slug"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
			State       *string `json:"state"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		project, err := s.service.UpdateProject(r.Context(), projectID, store.ProjectUpdate{Slug: input.Slug, Title: input.Title, Description: input.Description, State: input.State})
		writeStoreResult(w, http.StatusOK, project, err)
	case http.MethodDelete:
		writeDeleteResult(w, s.service.DeleteProject(r.Context(), projectID))
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleProjectTasks(w http.ResponseWriter, r *http.Request, projectID string) {
	switch r.Method {
	case http.MethodGet:
		filter := store.TaskFilter{Status: r.URL.Query().Get("status"), Priority: r.URL.Query().Get("priority"), Label: r.URL.Query().Get("label"), Query: r.URL.Query().Get("q"), AssigneeID: r.URL.Query().Get("assignee_id")}
		tasks, err := s.service.Store().ListProjectTasks(r.Context(), projectID, filter)
		writeStoreResult(w, http.StatusOK, tasks, err)
	case http.MethodPost:
		var input struct {
			ParentTaskID *string  `json:"parent_task_id"`
			Title        string   `json:"title"`
			Description  string   `json:"description"`
			Status       string   `json:"status"`
			Priority     string   `json:"priority"`
			AssigneeID   *string  `json:"assignee_id"`
			Labels       []string `json:"labels"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		task, err := s.service.CreateTask(r.Context(), store.TaskCreate{ProjectID: projectID, ParentTaskID: input.ParentTaskID, Title: input.Title, Description: input.Description, Status: input.Status, Priority: input.Priority, AssigneeID: input.AssigneeID, Labels: input.Labels})
		writeStoreResult(w, http.StatusCreated, task, err)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleTaskSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 {
		writeNotFound(w)
		return
	}
	taskID := parts[3]
	if len(parts) == 4 {
		s.handleTask(w, r, taskID)
		return
	}
	if len(parts) == 5 && parts[4] == "comments" {
		s.handleTaskComments(w, r, taskID)
		return
	}
	if len(parts) == 5 && parts[4] == "subtasks" {
		s.handleTaskSubtasks(w, r, taskID)
		return
	}
	if len(parts) == 5 && parts[4] == "reparent" {
		s.handleTaskReparent(w, r, taskID)
		return
	}
	writeNotFound(w)
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request, taskID string) {
	switch r.Method {
	case http.MethodGet:
		task, err := s.service.Store().GetTask(r.Context(), taskID)
		writeStoreResult(w, http.StatusOK, task, err)
	case http.MethodPatch:
		var input struct {
			ParentTaskID **string  `json:"parent_task_id"`
			Title        *string   `json:"title"`
			Description  *string   `json:"description"`
			Status       *string   `json:"status"`
			Priority     *string   `json:"priority"`
			AssigneeID   **string  `json:"assignee_id"`
			Labels       *[]string `json:"labels"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		updated, err := s.service.UpdateTask(r.Context(), taskID, store.TaskUpdate{ParentTaskID: input.ParentTaskID, Title: input.Title, Description: input.Description, Status: input.Status, Priority: input.Priority, AssigneeID: input.AssigneeID, Labels: input.Labels})
		writeStoreResult(w, http.StatusOK, updated, err)
	case http.MethodDelete:
		writeDeleteResult(w, s.service.DeleteTask(r.Context(), taskID))
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleTaskComments(w http.ResponseWriter, r *http.Request, taskID string) {
	switch r.Method {
	case http.MethodGet:
		comments, err := s.service.Store().ListTaskComments(r.Context(), taskID)
		writeStoreResult(w, http.StatusOK, comments, err)
	case http.MethodPost:
		var input struct {
			AuthorID *string `json:"author_id"`
			Body     string  `json:"body"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		comment, err := s.service.CreateComment(r.Context(), store.CommentCreate{TaskID: taskID, AuthorID: input.AuthorID, Body: input.Body})
		writeStoreResult(w, http.StatusCreated, comment, err)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleTaskSubtasks(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	parent, err := s.service.Store().GetTask(r.Context(), taskID)
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	var input struct {
		Tasks []struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Status      string   `json:"status"`
			Priority    string   `json:"priority"`
			AssigneeID  *string  `json:"assignee_id"`
			Labels      []string `json:"labels"`
		} `json:"tasks"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	created := make([]domain.Task, 0, len(input.Tasks))
	for _, item := range input.Tasks {
		parentID := taskID
		task, createErr := s.service.CreateTask(r.Context(), store.TaskCreate{ProjectID: parent.ProjectID, ParentTaskID: &parentID, Title: item.Title, Description: item.Description, Status: item.Status, Priority: item.Priority, AssigneeID: item.AssigneeID, Labels: item.Labels})
		if createErr != nil {
			writeError(w, http.StatusBadRequest, createErr.Error())
			return
		}
		created = append(created, task)
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleTaskReparent(w http.ResponseWriter, r *http.Request, taskID string) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	var input struct {
		ParentTaskID *string `json:"parent_task_id"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	task, err := s.service.ReparentTask(r.Context(), taskID, input.ParentTaskID)
	writeStoreResult(w, http.StatusOK, task, err)
}

func (s *Server) handleCommentSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 {
		writeNotFound(w)
		return
	}
	commentID := parts[3]
	switch r.Method {
	case http.MethodPatch:
		var input struct {
			Body string `json:"body"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		comment, err := s.service.UpdateComment(r.Context(), commentID, store.CommentUpdate{Body: input.Body})
		writeStoreResult(w, http.StatusOK, comment, err)
	case http.MethodDelete:
		writeDeleteResult(w, s.service.DeleteComment(r.Context(), commentID))
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		sessions, err := s.service.Store().ListSessions(r.Context())
		writeStoreResult(w, http.StatusOK, sessions, err)
	case http.MethodPost:
		var input struct {
			SnapshotID   string `json:"snapshot_id"`
			Type         string `json:"type"`
			Title        string `json:"title"`
			Description  string `json:"description"`
			ArtifactName string `json:"artifact_name"`
			ArtifactPath string `json:"artifact_path"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.SnapshotID == "" {
			input.SnapshotID = uuid.NewString()
		}
		session, err := s.service.CreateSession(r.Context(), store.SessionCreate{SnapshotID: input.SnapshotID, Type: input.Type, Title: input.Title, Description: input.Description, ArtifactName: input.ArtifactName, ArtifactPath: input.ArtifactPath})
		writeStoreResult(w, http.StatusCreated, session, err)
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleSessionUpload(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defer file.Close()

	artifactPath, err := s.storeArtifact(file, header)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"artifact_name": header.Filename, "artifact_path": artifactPath})
}

func (s *Server) handleSessionSubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) < 4 {
		writeNotFound(w)
		return
	}
	snapshotID := parts[3]
	if len(parts) == 5 && parts[4] == "download" {
		s.handleSessionDownload(w, r, snapshotID)
		return
	}
	switch r.Method {
	case http.MethodGet:
		session, err := s.service.Store().GetSession(r.Context(), snapshotID)
		writeStoreResult(w, http.StatusOK, session, err)
	case http.MethodPatch:
		var input struct {
			Title        *string `json:"title"`
			Description  *string `json:"description"`
			ArtifactName *string `json:"artifact_name"`
			ArtifactPath *string `json:"artifact_path"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		session, err := s.service.UpdateSession(r.Context(), snapshotID, store.SessionUpdate{Title: input.Title, Description: input.Description, ArtifactName: input.ArtifactName, ArtifactPath: input.ArtifactPath})
		writeStoreResult(w, http.StatusOK, session, err)
	case http.MethodDelete:
		writeDeleteResult(w, s.service.DeleteSession(r.Context(), snapshotID))
	default:
		writeMethodNotAllowed(w)
	}
}

func (s *Server) handleSessionDownload(w http.ResponseWriter, r *http.Request, snapshotID string) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	session, err := s.service.Store().GetSession(r.Context(), snapshotID)
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	if strings.HasPrefix(session.ArtifactPath, "s3://") {
		writeError(w, http.StatusNotImplemented, "s3 artifact download redirect is not implemented yet")
		return
	}
	http.ServeFile(w, r, session.ArtifactPath)
}

func (s *Server) handleActivities(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	filter := store.ActivityFilter{
		Project:    r.URL.Query().Get("project"),
		Task:       r.URL.Query().Get("task"),
		Label:      r.URL.Query().Get("label"),
		NotProject: parseBool(r.URL.Query().Get("not_project")),
		NotTask:    parseBool(r.URL.Query().Get("not_task")),
		NotLabel:   parseBool(r.URL.Query().Get("not_label")),
		Sort:       r.URL.Query().Get("sort"),
		Limit:      limit,
	}
	activities, err := s.service.Store().ListActivities(r.Context(), filter)
	writeStoreResult(w, http.StatusOK, activities, err)
}

func (s *Server) storeArtifact(file multipart.File, header *multipart.FileHeader) (string, error) {
	if err := os.MkdirAll(s.artifactsDir, 0o755); err != nil {
		return "", err
	}
	name := uuid.NewString() + "-" + filepath.Base(header.Filename)
	path := filepath.Join(s.artifactsDir, name)
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := out.ReadFrom(file); err != nil {
		return "", err
	}
	return path, nil
}

func splitPath(path string) []string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

func parseBool(value string) bool {
	parsed, _ := strconv.ParseBool(value)
	return parsed
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeStoreResult(w http.ResponseWriter, status int, payload any, err error) {
	if err == nil {
		writeJSON(w, status, payload)
		return
	}
	if errors.Is(err, bunstore.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func writeDeleteResult(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeStoreResult(w, http.StatusNoContent, nil, err)
}

func writeMethodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not found")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
