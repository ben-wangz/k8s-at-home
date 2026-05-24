package main

import "net/http"

type config struct {
	BaseURL string
	APIKey  string
	Raw     bool
}

type client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

type apiError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type uploadResponse struct {
	ArtifactName string `json:"artifact_name"`
	ArtifactPath string `json:"artifact_path"`
}

type project struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Open        int    `json:"open"`
	InProgress  int    `json:"in_progress"`
	InReview    int    `json:"in_review"`
}

type task struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	ParentTaskID *string  `json:"parent_task_id,omitempty"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	AssigneeID   *string  `json:"assignee_id,omitempty"`
	Labels       []string `json:"labels"`
}

type session struct {
	SnapshotID   string `json:"snapshot_id"`
	Type         string `json:"type"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	ArtifactName string `json:"artifact_name"`
	ArtifactPath string `json:"artifact_path"`
}

type activity struct {
	ID        string  `json:"id"`
	ProjectID *string `json:"project_id,omitempty"`
	TaskID    *string `json:"task_id,omitempty"`
	Label     string  `json:"label"`
	Kind      string  `json:"kind"`
	Message   string  `json:"message"`
}
