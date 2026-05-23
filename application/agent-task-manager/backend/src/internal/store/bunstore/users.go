package bunstore

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows := make([]userRow, 0)
	if err := s.db.NewSelect().Model(&rows).Order("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	users := make([]domain.User, 0, len(rows))
	for _, row := range rows {
		users = append(users, toUser(row))
	}
	return users, nil
}

func (s *Store) CreateUser(ctx context.Context, input store.UserCreate) (domain.User, error) {
	now := s.now()
	row := userRow{ID: uuid.NewString(), Email: input.Email, Name: input.Name, Role: defaultString(input.Role, "member"), Active: input.Active, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return toUser(row), err
}

func (s *Store) UpdateUser(ctx context.Context, id string, input store.UserUpdate) (domain.User, error) {
	row := new(userRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, ErrNotFound
		}
		return domain.User{}, err
	}
	row.Email = derefString(input.Email, row.Email)
	row.Name = derefString(input.Name, row.Name)
	row.Role = derefString(input.Role, row.Role)
	row.Active = derefBool(input.Active, row.Active)
	row.UpdatedAt = s.now()
	_, err := s.db.NewUpdate().Model(row).WherePK().Column("email", "name", "role", "active", "updated_at").Exec(ctx)
	return toUser(*row), err
}

func (s *Store) GetUserByAPIKey(ctx context.Context, token string) (domain.User, error) {
	row := new(userRow)
	hash := hashToken(token)
	err := s.db.NewSelect().Model(row).
		Join("JOIN api_keys ON api_keys.user_id = users.id").
		Where("api_keys.key_hash = ?", hash).
		Where("api_keys.revoked_at IS NULL").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.User{}, ErrNotFound
		}
		return domain.User{}, err
	}
	_, _ = s.db.NewUpdate().Model((*apiKeyRow)(nil)).Set("last_used_at = ?", s.now()).Where("key_hash = ?", hash).Exec(ctx)
	return toUser(*row), nil
}

func (s *Store) ListAPIKeys(ctx context.Context, userID string) ([]domain.APIKey, error) {
	rows := make([]apiKeyRow, 0)
	if err := s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).Order("created_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	keys := make([]domain.APIKey, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, domain.APIKey{ID: row.ID, UserID: row.UserID, Name: row.Name, Prefix: row.Prefix, CreatedAt: row.CreatedAt, LastUsedAt: row.LastUsedAt, RevokedAt: row.RevokedAt})
	}
	return keys, nil
}

func (s *Store) CreateAPIKey(ctx context.Context, input store.APIKeyCreate) (domain.APIKeySecret, error) {
	now := s.now()
	token, prefix := newAPIKeyToken()
	row := apiKeyRow{ID: uuid.NewString(), UserID: input.UserID, Name: input.Name, Prefix: prefix, KeyHash: hashToken(token), CreatedAt: now}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return domain.APIKeySecret{APIKey: domain.APIKey{ID: row.ID, UserID: row.UserID, Name: row.Name, Prefix: row.Prefix, CreatedAt: row.CreatedAt}, Token: token}, err
}

func (s *Store) DeleteAPIKey(ctx context.Context, userID, keyID string) error {
	result, err := s.db.NewUpdate().Model((*apiKeyRow)(nil)).Set("revoked_at = ?", s.now()).Where("id = ?", keyID).Where("user_id = ?", userID).Where("revoked_at IS NULL").Exec(ctx)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	return expectRowsAffected(affected, err)
}
