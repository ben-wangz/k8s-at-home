package bunstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/driver/pgdriver"
	_ "modernc.org/sqlite"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/config"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

var slugPattern = regexp.MustCompile(`[^a-z0-9-]+`)

var ErrNotFound = errors.New("not found")

type Store struct {
	sqlDB      *sql.DB
	db         *bun.DB
	driver     string
	readyQuery string
}

type userRow struct {
	bun.BaseModel `bun:"table:users"`
	ID            string    `bun:",pk" json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at" json:"updated_at"`
}

type apiKeyRow struct {
	bun.BaseModel `bun:"table:api_keys"`
	ID            string     `bun:",pk" json:"id"`
	UserID        string     `bun:"user_id" json:"user_id"`
	Name          string     `json:"name"`
	Prefix        string     `json:"prefix"`
	KeyHash       string     `bun:"key_hash" json:"-"`
	CreatedAt     time.Time  `bun:"created_at" json:"created_at"`
	LastUsedAt    *time.Time `bun:"last_used_at" json:"last_used_at,omitempty"`
	RevokedAt     *time.Time `bun:"revoked_at" json:"revoked_at,omitempty"`
}

type projectRow struct {
	bun.BaseModel `bun:"table:projects"`
	ID            string    `bun:",pk" json:"id"`
	Slug          string    `json:"slug"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	State         string    `json:"state"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at" json:"updated_at"`
}

type taskRow struct {
	bun.BaseModel `bun:"table:tasks"`
	ID            string    `bun:",pk" json:"id"`
	ProjectID     string    `bun:"project_id" json:"project_id"`
	ParentTaskID  *string   `bun:"parent_task_id" json:"parent_task_id,omitempty"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Status        string    `json:"status"`
	Priority      string    `json:"priority"`
	AssigneeID    *string   `bun:"assignee_id" json:"assignee_id,omitempty"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at" json:"updated_at"`
}

type taskLabelRow struct {
	bun.BaseModel `bun:"table:task_labels"`
	TaskID        string `bun:"task_id,pk"`
	Label         string `bun:"label,pk"`
}

type commentRow struct {
	bun.BaseModel `bun:"table:task_comments"`
	ID            string    `bun:",pk" json:"id"`
	TaskID        string    `bun:"task_id" json:"task_id"`
	AuthorID      *string   `bun:"author_id" json:"author_id,omitempty"`
	Body          string    `json:"body"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at" json:"updated_at"`
}

type sessionRow struct {
	bun.BaseModel `bun:"table:sessions"`
	SnapshotID    string    `bun:"snapshot_id,pk" json:"snapshot_id"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	ArtifactName  string    `bun:"artifact_name" json:"artifact_name"`
	ArtifactPath  string    `bun:"artifact_path" json:"artifact_path"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at" json:"updated_at"`
}

type activityRow struct {
	bun.BaseModel `bun:"table:activities"`
	ID            string    `bun:",pk" json:"id"`
	ProjectID     *string   `bun:"project_id" json:"project_id,omitempty"`
	TaskID        *string   `bun:"task_id" json:"task_id,omitempty"`
	Label         string    `json:"label"`
	Kind          string    `json:"kind"`
	Message       string    `json:"message"`
	CreatedAt     time.Time `bun:"created_at" json:"created_at"`
}

func Open(cfg config.DatabaseConfig) (*Store, error) {
	sqlDB, db, err := openBunDB(cfg)
	if err != nil {
		return nil, err
	}
	store := &Store{sqlDB: sqlDB, db: db, driver: cfg.Driver, readyQuery: cfg.ReadyQuery}
	if err := store.init(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return store, nil
}

func openBunDB(cfg config.DatabaseConfig) (*sql.DB, *bun.DB, error) {
	if strings.EqualFold(cfg.Driver, "postgres") {
		sqldb := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(cfg.DSN)))
		return sqldb, bun.NewDB(sqldb, pgdialect.New()), nil
	}
	sqldb, err := sql.Open("sqlite", cfg.DSN)
	if err != nil {
		return nil, nil, err
	}
	return sqldb, bun.NewDB(sqldb, sqlitedialect.New()), nil
}

func (s *Store) Close() error {
	return s.sqlDB.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	if err := s.sqlDB.PingContext(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, s.readyQuery)
	return err
}

func (s *Store) init(ctx context.Context) error {
	for _, stmt := range schemaStatements(s.driver) {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) now() time.Time {
	return time.Now().UTC()
}

func normalizeSlug(value string) string {
	slug := strings.Trim(strings.ToLower(value), "-")
	slug = strings.ReplaceAll(slug, " ", "-")
	slug = slugPattern.ReplaceAllString(slug, "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return uuid.NewString()
	}
	return slug
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newAPIKeyToken() (token string, prefix string) {
	token = "atm_" + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	prefix = token
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return token, prefix
}

func derefString(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

func derefBool(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func cleanLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	cleaned := make([]string, 0, len(labels))
	for _, label := range labels {
		value := strings.TrimSpace(label)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

func expectRowsAffected(affected int64, err error) error {
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound)
}

func toUser(row userRow) domain.User {
	return domain.User{
		ID:        row.ID,
		Email:     row.Email,
		Name:      row.Name,
		Role:      row.Role,
		Active:    row.Active,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func toProject(row projectRow) domain.Project {
	return domain.Project{
		ID:          row.ID,
		Slug:        row.Slug,
		Title:       row.Title,
		Description: row.Description,
		State:       row.State,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func toComment(row commentRow) domain.Comment {
	return domain.Comment{
		ID:        row.ID,
		TaskID:    row.TaskID,
		AuthorID:  row.AuthorID,
		Body:      row.Body,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func toSession(row sessionRow) domain.Session {
	return domain.Session{
		SnapshotID:   row.SnapshotID,
		Type:         row.Type,
		Title:        row.Title,
		Description:  row.Description,
		ArtifactName: row.ArtifactName,
		ArtifactPath: row.ArtifactPath,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

func toActivity(row activityRow) domain.Activity {
	return domain.Activity{
		ID:        row.ID,
		ProjectID: row.ProjectID,
		TaskID:    row.TaskID,
		Label:     row.Label,
		Kind:      row.Kind,
		Message:   row.Message,
		CreatedAt: row.CreatedAt,
	}
}

func toTask(row taskRow, labels []string) domain.Task {
	return domain.Task{
		ID:           row.ID,
		ProjectID:    row.ProjectID,
		ParentTaskID: row.ParentTaskID,
		Title:        row.Title,
		Description:  row.Description,
		Status:       row.Status,
		Priority:     row.Priority,
		AssigneeID:   row.AssigneeID,
		Labels:       labels,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
}

func (s *Store) labelMap(ctx context.Context, taskIDs []string) (map[string][]string, error) {
	if len(taskIDs) == 0 {
		return map[string][]string{}, nil
	}
	rows := make([]taskLabelRow, 0)
	err := s.db.NewSelect().Model(&rows).Where("task_id IN (?)", bun.In(taskIDs)).Order("label ASC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	labelsByTask := make(map[string][]string, len(taskIDs))
	for _, row := range rows {
		labelsByTask[row.TaskID] = append(labelsByTask[row.TaskID], row.Label)
	}
	return labelsByTask, nil
}

func (s *Store) replaceLabels(ctx context.Context, taskID string, labels []string) error {
	if _, err := s.db.NewDelete().Model((*taskLabelRow)(nil)).Where("task_id = ?", taskID).Exec(ctx); err != nil {
		return err
	}
	rows := make([]taskLabelRow, 0, len(labels))
	for _, label := range cleanLabels(labels) {
		rows = append(rows, taskLabelRow{TaskID: taskID, Label: label})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := s.db.NewInsert().Model(&rows).Exec(ctx)
	return err
}

func schemaStatements(driver string) []string {
	if driver == "postgres" {
		return postgresSchema()
	}
	return sqliteSchema()
}

func postgresSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL, role TEXT NOT NULL, active BOOLEAN NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS api_keys (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, name TEXT NOT NULL, prefix TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, created_at TIMESTAMPTZ NOT NULL, last_used_at TIMESTAMPTZ NULL, revoked_at TIMESTAMPTZ NULL)`,
		`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, title TEXT NOT NULL, description TEXT NOT NULL, state TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, parent_task_id TEXT NULL REFERENCES tasks(id) ON DELETE SET NULL, title TEXT NOT NULL, description TEXT NOT NULL, status TEXT NOT NULL, priority TEXT NOT NULL, assignee_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS task_labels (task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, label TEXT NOT NULL, PRIMARY KEY(task_id, label))`,
		`CREATE TABLE IF NOT EXISTS task_comments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, author_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL, body TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (snapshot_id TEXT PRIMARY KEY, type TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL, artifact_name TEXT NOT NULL, artifact_path TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS activities (id TEXT PRIMARY KEY, project_id TEXT NULL REFERENCES projects(id) ON DELETE CASCADE, task_id TEXT NULL REFERENCES tasks(id) ON DELETE CASCADE, label TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL)`,
	}
}

func sqliteSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE, name TEXT NOT NULL, role TEXT NOT NULL, active BOOLEAN NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS api_keys (id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE, name TEXT NOT NULL, prefix TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, created_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP NULL, revoked_at TIMESTAMP NULL)`,
		`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, title TEXT NOT NULL, description TEXT NOT NULL, state TEXT NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE, parent_task_id TEXT NULL REFERENCES tasks(id) ON DELETE SET NULL, title TEXT NOT NULL, description TEXT NOT NULL, status TEXT NOT NULL, priority TEXT NOT NULL, assignee_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS task_labels (task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, label TEXT NOT NULL, PRIMARY KEY(task_id, label))`,
		`CREATE TABLE IF NOT EXISTS task_comments (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, author_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL, body TEXT NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions (snapshot_id TEXT PRIMARY KEY, type TEXT NOT NULL, title TEXT NOT NULL, description TEXT NOT NULL, artifact_name TEXT NOT NULL, artifact_path TEXT NOT NULL, created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS activities (id TEXT PRIMARY KEY, project_id TEXT NULL REFERENCES projects(id) ON DELETE CASCADE, task_id TEXT NULL REFERENCES tasks(id) ON DELETE CASCADE, label TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL, created_at TIMESTAMP NOT NULL)`,
	}
}

var _ store.Store = (*Store)(nil)
