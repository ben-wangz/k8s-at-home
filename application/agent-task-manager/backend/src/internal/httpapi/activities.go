package httpapi

import (
	"net/http"
	"strconv"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

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
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[domain.Activity]{Items: activities})
}

func (s *Server) handleActivitySubresources(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	parts := splitPath(r.URL.Path)
	if len(parts) != 4 {
		writeNotFound(w)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	activity, err := s.service.Store().GetActivity(r.Context(), parts[3])
	writeStoreResult(w, http.StatusOK, activity, err)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request, _ *domain.User) {
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	filter := store.TaskFilter{Query: r.URL.Query().Get("q"), Label: r.URL.Query().Get("label"), Status: r.URL.Query().Get("status"), Priority: r.URL.Query().Get("priority"), AssigneeID: r.URL.Query().Get("assignee_id")}
	tasks, err := s.service.Store().ListTasks(r.Context(), filter)
	if err != nil {
		writeStoreResult(w, http.StatusOK, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, listResponse[domain.Task]{Items: tasks})
}
