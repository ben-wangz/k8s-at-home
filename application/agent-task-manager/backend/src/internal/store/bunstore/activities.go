package bunstore

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListActivities(ctx context.Context, filter store.ActivityFilter) ([]domain.Activity, error) {
	rows := make([]activityRow, 0)
	query := s.db.NewSelect().Model(&rows)
	if filter.Project != "" {
		addFilter(query, "project_id", filter.Project, filter.NotProject)
	}
	if filter.Task != "" {
		addFilter(query, "task_id", filter.Task, filter.NotTask)
	}
	if filter.Label != "" {
		addFilter(query, "label", filter.Label, filter.NotLabel)
	}
	if strings.EqualFold(filter.Sort, "asc") {
		query.Order("created_at ASC")
	} else {
		query.Order("created_at DESC")
	}
	if filter.Limit > 0 {
		query.Limit(filter.Limit)
	}
	if err := query.Scan(ctx); err != nil {
		return nil, err
	}
	activities := make([]domain.Activity, 0, len(rows))
	for _, row := range rows {
		activities = append(activities, toActivity(row))
	}
	return activities, nil
}

func addFilter(query *bun.SelectQuery, column, value string, negate bool) {
	operator := "="
	if negate {
		operator = "<>"
	}
	query.Where(column+" "+operator+" ?", value)
}

func (s *Store) CreateActivity(ctx context.Context, input domain.Activity) (domain.Activity, error) {
	row := activityRow{
		ID:        input.ID,
		ProjectID: input.ProjectID,
		TaskID:    input.TaskID,
		Label:     input.Label,
		Kind:      input.Kind,
		Message:   input.Message,
		CreatedAt: input.CreatedAt,
	}
	if row.ID == "" {
		row.ID = uuid.NewString()
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = s.now()
	}
	_, err := s.db.NewInsert().Model(&row).Exec(ctx)
	return toActivity(row), err
}
