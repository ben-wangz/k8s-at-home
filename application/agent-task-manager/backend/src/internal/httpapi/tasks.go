package httpapi

import (
	"net/http"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	filter := store.TaskFilter{Status: r.URL.Query().Get("status"), Priority: r.URL.Query().Get("priority"), Label: r.URL.Query().Get("label"), Query: r.URL.Query().Get("q"), AssigneeID: r.URL.Query().Get("assignee_id")}
	tasks, err := s.service.Store().ListTasks(r.Context(), filter)
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[domain.Task]{Items: tasks})
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
		if err != nil {
			writeStoreResult(w, http.StatusOK, nil, err)
			return
		}
		writeJSON(w, http.StatusOK, listResponse[domain.Comment]{Items: comments})
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
			writeError(w, http.StatusBadRequest, "invalid_request", createErr.Error())
			return
		}
		created = append(created, task)
	}
	writeJSON(w, http.StatusCreated, listResponse[domain.Task]{Items: created})
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
		var input struct { Body string `json:"body"` }
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
