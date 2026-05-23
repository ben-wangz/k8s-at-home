package bunstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListSessions(ctx context.Context) ([]domain.Session, error) {
	rows := make([]sessionRow, 0)
	if err := s.db.NewSelect().Model(&rows).Order("updated_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	sessions := make([]domain.Session, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, toSession(row))
	}
	return sessions, nil
}

func (s *Store) CreateSession(ctx context.Context, input store.SessionCreate) (domain.Session, error) {
	now := s.now()
	row := sessionRow{SnapshotID: input.SnapshotID, Type: defaultString(input.Type, "opencode"), Title: input.Title, Description: input.Description, ArtifactName: input.ArtifactName, ArtifactPath: input.ArtifactPath, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return toSession(row), err
}

func (s *Store) GetSession(ctx context.Context, snapshotID string) (domain.Session, error) {
	row := new(sessionRow)
	if err := s.db.NewSelect().Model(row).Where("snapshot_id = ?", snapshotID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Session{}, ErrNotFound
		}
		return domain.Session{}, err
	}
	return toSession(*row), nil
}

func (s *Store) UpdateSession(ctx context.Context, snapshotID string, input store.SessionUpdate) (domain.Session, error) {
	row := new(sessionRow)
	if err := s.db.NewSelect().Model(row).Where("snapshot_id = ?", snapshotID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Session{}, ErrNotFound
		}
		return domain.Session{}, err
	}
	row.Title = derefString(input.Title, row.Title)
	row.Description = derefString(input.Description, row.Description)
	row.ArtifactName = derefString(input.ArtifactName, row.ArtifactName)
	row.ArtifactPath = derefString(input.ArtifactPath, row.ArtifactPath)
	row.UpdatedAt = s.now()
	_, err := s.db.NewUpdate().Model(row).WherePK().Column("title", "description", "artifact_name", "artifact_path", "updated_at").Exec(ctx)
	return toSession(*row), err
}

func (s *Store) DeleteSession(ctx context.Context, snapshotID string) error {
	result, err := s.db.NewDelete().Model((*sessionRow)(nil)).Where("snapshot_id = ?", snapshotID).Exec(ctx)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	return expectRowsAffected(affected, err)
}
