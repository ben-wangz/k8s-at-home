package bunstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListProjects(ctx context.Context) ([]domain.Project, error) {
	rows := make([]projectRow, 0)
	if err := s.db.NewSelect().Model(&rows).Order("updated_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	projects := make([]domain.Project, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, s.decorateProjectCounts(ctx, toProject(row)))
	}
	return projects, nil
}

func (s *Store) CreateProject(ctx context.Context, input store.ProjectCreate) (domain.Project, error) {
	now := s.now()
	row := projectRow{ID: uuid.NewString(), Slug: normalizeSlug(input.Slug), Title: input.Title, Description: input.Description, State: defaultString(input.State, "active"), CreatedAt: now, UpdatedAt: now}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return toProject(row), err
}

func (s *Store) GetProject(ctx context.Context, id string) (domain.Project, error) {
	row := new(projectRow)
	err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Project{}, ErrNotFound
		}
		return domain.Project{}, err
	}
	return s.decorateProjectCounts(ctx, toProject(*row)), nil
}

func (s *Store) UpdateProject(ctx context.Context, id string, input store.ProjectUpdate) (domain.Project, error) {
	row := new(projectRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Project{}, ErrNotFound
		}
		return domain.Project{}, err
	}
	row.Slug = normalizeSlug(derefString(input.Slug, row.Slug))
	row.Title = derefString(input.Title, row.Title)
	row.Description = derefString(input.Description, row.Description)
	row.State = derefString(input.State, row.State)
	row.UpdatedAt = s.now()
	_, err := s.db.NewUpdate().Model(row).WherePK().Column("slug", "title", "description", "state", "updated_at").Exec(ctx)
	return toProject(*row), err
}

func (s *Store) DeleteProject(ctx context.Context, id string) error {
	result, err := s.db.NewDelete().Model((*projectRow)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	return expectRowsAffected(affected, err)
}

func (s *Store) decorateProjectCounts(ctx context.Context, project domain.Project) domain.Project {
	tasks, err := s.ListProjectTasks(ctx, project.ID, store.TaskFilter{})
	if err != nil {
		return project
	}
	for _, task := range tasks {
		switch task.Status {
		case "in_progress":
			project.InProgress++
		case "in_review":
			project.InReview++
		case "done":
			project.Done++
		case "cancelled":
			project.Cancelled++
		default:
			project.Open++
		}
	}
	return project
}

func (s *Store) GetProjectOverview(ctx context.Context, projectID string) (store.ProjectOverview, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return store.ProjectOverview{}, err
	}
	tasks, err := s.ListProjectTasks(ctx, projectID, store.TaskFilter{})
	if err != nil {
		return store.ProjectOverview{}, err
	}
	activities, err := s.ListActivities(ctx, store.ActivityFilter{Project: projectID, Sort: "desc", Limit: 10})
	if err != nil {
		return store.ProjectOverview{}, err
	}
	sessions, err := s.ListSessions(ctx)
	if err != nil {
		return store.ProjectOverview{}, err
	}
	overview := store.ProjectOverview{Project: project}
	for _, task := range tasks {
		switch task.Status {
		case "in_progress":
			overview.InProgress++
		case "in_review":
			overview.InReview++
		case "done":
			overview.Done++
		case "cancelled":
			overview.Cancelled++
		default:
			overview.Open++
		}
	}
	if len(tasks) > 4 {
		overview.RecentTasks = tasks[:4]
	} else {
		overview.RecentTasks = tasks
	}
	if len(sessions) > 4 {
		overview.RecentSessions = sessions[:4]
	} else {
		overview.RecentSessions = sessions
	}
	overview.RecentActivities = activities
	return overview, nil
}
