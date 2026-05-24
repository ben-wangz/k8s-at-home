package httpapi

import (
	"net/http"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	switch r.Method {
	case http.MethodGet:
		projects, err := s.service.Store().ListProjects(r.Context())
		if err != nil {
			writeStoreResult(w, http.StatusOK, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.Project]{Items: projects})
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
	if len(parts) == 5 && parts[4] == "overview" {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		overview, err := s.service.GetProjectOverview(r.Context(), projectID)
		writeStoreResult(w, http.StatusOK, overview, err)
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
		if err != nil {
			writeStoreResult(w, http.StatusOK, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.Task]{Items: tasks})
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
