package store

import (
	"context"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
)

type TaskFilter struct {
	Status     string
	Priority   string
	Label      string
	Query      string
	AssigneeID string
}

type ActivityFilter struct {
	Project    string
	Task       string
	Label      string
	NotProject bool
	NotTask    bool
	NotLabel   bool
	Sort       string
	Limit      int
}

type ProjectCreate struct {
	Slug        string
	Title       string
	Description string
	State       string
}

type ProjectUpdate struct {
	Slug        *string
	Title       *string
	Description *string
	State       *string
}

type TaskCreate struct {
	ProjectID    string
	ParentTaskID *string
	Title        string
	Description  string
	Status       string
	Priority     string
	AssigneeID   *string
	Labels       []string
}

type TaskUpdate struct {
	ParentTaskID **string
	Title        *string
	Description  *string
	Status       *string
	Priority     *string
	AssigneeID   **string
	Labels       *[]string
}

type CommentCreate struct {
	TaskID   string
	AuthorID *string
	Body     string
}

type CommentUpdate struct {
	Body string
}

type SessionCreate struct {
	SnapshotID   string
	Type         string
	Title        string
	Description  string
	ArtifactName string
	ArtifactPath string
}

type SessionUpdate struct {
	Title        *string
	Description  *string
	ArtifactName *string
	ArtifactPath *string
}

type UserCreate struct {
	Email  string
	Name   string
	Role   string
	Active bool
}

type UserUpdate struct {
	Email  *string
	Name   *string
	Role   *string
	Active *bool
}

type APIKeyCreate struct {
	UserID string
	Name   string
}

type ProjectOverview struct {
	Project          domain.Project   `json:"project"`
	Open             int              `json:"open"`
	InProgress       int              `json:"in_progress"`
	InReview         int              `json:"in_review"`
	Done             int              `json:"done"`
	Cancelled        int              `json:"cancelled"`
	RecentTasks      []domain.Task    `json:"recent_tasks"`
	RecentSessions   []domain.Session `json:"recent_sessions"`
	RecentActivities []domain.Activity `json:"recent_activities"`
}

type Store interface {
	Ping(context.Context) error
	Close() error

	ListProjects(context.Context) ([]domain.Project, error)
	CreateProject(context.Context, ProjectCreate) (domain.Project, error)
	GetProject(context.Context, string) (domain.Project, error)
	GetProjectOverview(context.Context, string) (ProjectOverview, error)
	UpdateProject(context.Context, string, ProjectUpdate) (domain.Project, error)
	DeleteProject(context.Context, string) error

	ListTasks(context.Context, TaskFilter) ([]domain.Task, error)
	ListProjectTasks(context.Context, string, TaskFilter) ([]domain.Task, error)
	CreateTask(context.Context, TaskCreate) (domain.Task, error)
	GetTask(context.Context, string) (domain.TaskDetail, error)
	UpdateTask(context.Context, string, TaskUpdate) (domain.Task, error)
	ReparentTask(context.Context, string, *string) (domain.Task, error)
	DeleteTask(context.Context, string) error

	ListTaskComments(context.Context, string) ([]domain.Comment, error)
	CreateComment(context.Context, CommentCreate) (domain.Comment, error)
	UpdateComment(context.Context, string, CommentUpdate) (domain.Comment, error)
	DeleteComment(context.Context, string) error

	ListSessions(context.Context) ([]domain.Session, error)
	CreateSession(context.Context, SessionCreate) (domain.Session, error)
	GetSession(context.Context, string) (domain.Session, error)
	UpdateSession(context.Context, string, SessionUpdate) (domain.Session, error)
	DeleteSession(context.Context, string) error

	ListActivities(context.Context, ActivityFilter) ([]domain.Activity, error)
	GetActivity(context.Context, string) (domain.Activity, error)
	CreateActivity(context.Context, domain.Activity) (domain.Activity, error)

	ListUsers(context.Context) ([]domain.User, error)
	CreateUser(context.Context, UserCreate) (domain.User, error)
	UpdateUser(context.Context, string, UserUpdate) (domain.User, error)
	GetUserByAPIKey(context.Context, string) (domain.User, error)
	ListAPIKeys(context.Context, string) ([]domain.APIKey, error)
	CreateAPIKey(context.Context, APIKeyCreate) (domain.APIKeySecret, error)
	DeleteAPIKey(context.Context, string, string) error
}
