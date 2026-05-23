package bunstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

func (s *Store) ListProjectTasks(ctx context.Context, projectID string, filter store.TaskFilter) ([]domain.Task, error) {
	rows := make([]taskRow, 0)
	query := s.db.NewSelect().Model(&rows).Where("project_id = ?", projectID)
	if filter.Status != "" {
		query.Where("status = ?", filter.Status)
	}
	if filter.Priority != "" {
		query.Where("priority = ?", filter.Priority)
	}
	if filter.AssigneeID != "" {
		query.Where("assignee_id = ?", filter.AssigneeID)
	}
	if filter.Query != "" {
		pattern := "%" + strings.ToLower(filter.Query) + "%"
		query.Where("(LOWER(title) LIKE ? OR LOWER(description) LIKE ?)", pattern, pattern)
	}
	if filter.Label != "" {
		query.Where("id IN (?)", s.labelTaskSubquery(filter.Label))
	}
	if err := query.Order("updated_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	taskIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		taskIDs = append(taskIDs, row.ID)
	}
	labelsByTask, err := s.labelMap(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	tasks := make([]domain.Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, toTask(row, labelsByTask[row.ID]))
	}
	return tasks, nil
}

func (s *Store) labelTaskSubquery(label string) *bun.SelectQuery {
	return s.db.NewSelect().Model((*taskLabelRow)(nil)).Column("task_id").Where("label = ?", label)
}

func (s *Store) CreateTask(ctx context.Context, input store.TaskCreate) (domain.Task, error) {
	now := s.now()
	row := taskRow{ID: uuid.NewString(), ProjectID: input.ProjectID, ParentTaskID: input.ParentTaskID, Title: input.Title, Description: input.Description, Status: defaultString(input.Status, "todo"), Priority: defaultString(input.Priority, "medium"), AssigneeID: input.AssigneeID, CreatedAt: now, UpdatedAt: now}
	if _, err := s.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		return domain.Task{}, err
	}
	labels := cleanLabels(input.Labels)
	if err := s.replaceLabels(ctx, row.ID, labels); err != nil {
		return domain.Task{}, err
	}
	return toTask(row, labels), nil
}

func (s *Store) GetTask(ctx context.Context, id string) (domain.TaskDetail, error) {
	row := new(taskRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.TaskDetail{}, ErrNotFound
		}
		return domain.TaskDetail{}, err
	}
	labelsByTask, err := s.labelMap(ctx, []string{id})
	if err != nil {
		return domain.TaskDetail{}, err
	}
	task := toTask(*row, labelsByTask[id])
	subtasks, err := s.listChildTasks(ctx, id)
	if err != nil {
		return domain.TaskDetail{}, err
	}
	comments, err := s.ListTaskComments(ctx, id)
	if err != nil {
		return domain.TaskDetail{}, err
	}
	activities, err := s.ListActivities(ctx, store.ActivityFilter{Task: id, Sort: "desc", Limit: 50})
	if err != nil {
		return domain.TaskDetail{}, err
	}
	return domain.TaskDetail{Task: task, Subtasks: subtasks, Comments: comments, Activities: activities}, nil
}

func (s *Store) UpdateTask(ctx context.Context, id string, input store.TaskUpdate) (domain.Task, error) {
	row := new(taskRow)
	if err := s.db.NewSelect().Model(row).Where("id = ?", id).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Task{}, ErrNotFound
		}
		return domain.Task{}, err
	}
	if input.ParentTaskID != nil {
		row.ParentTaskID = *input.ParentTaskID
	}
	if input.AssigneeID != nil {
		row.AssigneeID = *input.AssigneeID
	}
	row.Title = derefString(input.Title, row.Title)
	row.Description = derefString(input.Description, row.Description)
	row.Status = derefString(input.Status, row.Status)
	row.Priority = derefString(input.Priority, row.Priority)
	row.UpdatedAt = s.now()
	if _, err := s.db.NewUpdate().Model(row).WherePK().Column("parent_task_id", "title", "description", "status", "priority", "assignee_id", "updated_at").Exec(ctx); err != nil {
		return domain.Task{}, err
	}
	labelsByTask, err := s.labelMap(ctx, []string{id})
	if err != nil {
		return domain.Task{}, err
	}
	labels := labelsByTask[id]
	if input.Labels != nil {
		labels = cleanLabels(*input.Labels)
		if err := s.replaceLabels(ctx, id, labels); err != nil {
			return domain.Task{}, err
		}
	}
	return toTask(*row, labels), nil
}

func (s *Store) ReparentTask(ctx context.Context, id string, parentID *string) (domain.Task, error) {
	return s.UpdateTask(ctx, id, store.TaskUpdate{ParentTaskID: &parentID})
}

func (s *Store) DeleteTask(ctx context.Context, id string) error {
	result, err := s.db.NewDelete().Model((*taskRow)(nil)).Where("id = ?", id).Exec(ctx)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	return expectRowsAffected(affected, err)
}

func (s *Store) listChildTasks(ctx context.Context, taskID string) ([]domain.Task, error) {
	rows := make([]taskRow, 0)
	if err := s.db.NewSelect().Model(&rows).Where("parent_task_id = ?", taskID).Order("updated_at DESC").Scan(ctx); err != nil {
		return nil, err
	}
	taskIDs := make([]string, 0, len(rows))
	for _, row := range rows {
		taskIDs = append(taskIDs, row.ID)
	}
	labelsByTask, err := s.labelMap(ctx, taskIDs)
	if err != nil {
		return nil, err
	}
	tasks := make([]domain.Task, 0, len(rows))
	for _, row := range rows {
		tasks = append(tasks, toTask(row, labelsByTask[row.ID]))
	}
	return tasks, nil
}
