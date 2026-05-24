package domain

import "time"

type User struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type APIKey struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type APIKeySecret struct {
	APIKey
	Token string `json:"token"`
}

type Project struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	State       string    `json:"state"`
	Open        int       `json:"open"`
	InProgress  int       `json:"in_progress"`
	InReview    int       `json:"in_review"`
	Done        int       `json:"done"`
	Cancelled   int       `json:"cancelled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Task struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ParentTaskID *string   `json:"parent_task_id,omitempty"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	Status       string    `json:"status"`
	Priority     string    `json:"priority"`
	AssigneeID   *string   `json:"assignee_id,omitempty"`
	Labels       []string  `json:"labels"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Comment struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	AuthorID  *string   `json:"author_id,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Session struct {
	SnapshotID   string    `json:"snapshot_id"`
	Type         string    `json:"type"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	ArtifactName string    `json:"artifact_name"`
	ArtifactPath string    `json:"artifact_path"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Activity struct {
	ID        string    `json:"id"`
	ProjectID *string   `json:"project_id,omitempty"`
	TaskID    *string   `json:"task_id,omitempty"`
	Label     string    `json:"label"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

type TaskDetail struct {
	Task
	Subtasks   []Task     `json:"subtasks"`
	Comments   []Comment  `json:"comments"`
	Activities []Activity `json:"activities"`
}
