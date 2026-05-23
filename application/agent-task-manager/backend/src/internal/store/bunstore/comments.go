package bunstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListTaskComments(ctx context.Context, taskID string) ([]domain.Comment, error) {
	rows := make([]commentRow, 0)
	if err := s.db.NewSelect().Model(&rows).Where("task_id = ?", taskID).Order("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	comments := make([]domain.Comment, 0, len(rows))
	for _, row := range rows {
		comments = append(comments, toComment(row))
	}
	return comments, nil
}

func (s *Store) CreateComment(ctx context.Context, input store.CommentCreate) (domain.Comment, error) {
	now := s.now()
	row := commentRow{ID: uuid.NewString(), TaskID: input.TaskID, AuthorID: input.AuthorID, Body: input.Body, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return toComment(row), err
}

func (s *Store) UpdateComment(ctx context.Context, id string, input store.CommentUpdate) (domain.Comment, error) {
	row := new(commentRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Comment{}, ErrNotFound
		}
		return domain.Comment{}, err
	}
	row.Body = input.Body
	row.UpdatedAt = s.now()
	_, err := s.db.NewUpdate().Model(row).WherePK().Column("body", "updated_at").Exec(ctx)
	return toComment(*row), err
}

func (s *Store) DeleteComment(ctx context.Context, id string) error {
	result, err := s.db.NewDelete().Model((*commentRow)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	return expectRowsAffected(affected, err)
}
