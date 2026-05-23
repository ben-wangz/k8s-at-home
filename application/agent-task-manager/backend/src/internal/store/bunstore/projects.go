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
		projects = append(projects, toProject(row))
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
	return toProject(*row), nil
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
