package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/domain"
	"github.com/ben-wangz/k8s-at-home/application/agent-task-manager/backend/src/internal/store"
)

type Service struct {
	store store.Store
}

func New(store store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) Store() store.Store {
	return s.store
}

func (s *Service) CreateProject(ctx context.Context, input store.ProjectCreate) (domain.Project, error) {
	project, err := s.store.CreateProject(ctx, input)
	if err != nil {
		return domain.Project{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{ID: uuid.NewString(), ProjectID: &project.ID, Label: "project", Kind: "project.created", Message: fmt.Sprintf("Project %s created", project.Title)})
	return project, nil
}

func (s *Service) UpdateProject(ctx context.Context, id string, input store.ProjectUpdate) (domain.Project, error) {
	project, err := s.store.UpdateProject(ctx, id, input)
	if err != nil {
		return domain.Project{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{ProjectID: &project.ID, Label: "project", Kind: "project.updated", Message: fmt.Sprintf("Project %s updated", project.Title)})
	return project, nil
}

func (s *Service) DeleteProject(ctx context.Context, id string) error {
	return s.store.DeleteProject(ctx, id)
}

func (s *Service) CreateTask(ctx context.Context, input store.TaskCreate) (domain.Task, error) {
	task, err := s.store.CreateTask(ctx, input)
	if err != nil {
		return domain.Task{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{ProjectID: &task.ProjectID, TaskID: &task.ID, Label: "task", Kind: "task.created", Message: fmt.Sprintf("Task %s created", task.Title)})
	return task, nil
}

func (s *Service) UpdateTask(ctx context.Context, id string, input store.TaskUpdate) (domain.Task, error) {
	task, err := s.store.UpdateTask(ctx, id, input)
	if err != nil {
		return domain.Task{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{ProjectID: &task.ProjectID, TaskID: &task.ID, Label: "task", Kind: "task.updated", Message: fmt.Sprintf("Task %s updated", task.Title)})
	return task, nil
}

func (s *Service) ReparentTask(ctx context.Context, id string, parentID *string) (domain.Task, error) {
	task, err := s.store.ReparentTask(ctx, id, parentID)
	if err != nil {
		return domain.Task{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{ProjectID: &task.ProjectID, TaskID: &task.ID, Label: "task", Kind: "task.reparented", Message: fmt.Sprintf("Task %s reparented", task.Title)})
	return task, nil
}

func (s *Service) DeleteTask(ctx context.Context, id string) error {
	return s.store.DeleteTask(ctx, id)
}

func (s *Service) CreateComment(ctx context.Context, input store.CommentCreate) (domain.Comment, error) {
	comment, err := s.store.CreateComment(ctx, input)
	if err != nil {
		return domain.Comment{}, err
	}
	detail, getErr := s.store.GetTask(ctx, input.TaskID)
	if getErr == nil {
		_, _ = s.store.CreateActivity(ctx, domain.Activity{ProjectID: &detail.ProjectID, TaskID: &detail.ID, Label: "comment", Kind: "comment.created", Message: "Comment added"})
	}
	return comment, nil
}

func (s *Service) UpdateComment(ctx context.Context, id string, input store.CommentUpdate) (domain.Comment, error) {
	return s.store.UpdateComment(ctx, id, input)
}

func (s *Service) DeleteComment(ctx context.Context, id string) error {
	return s.store.DeleteComment(ctx, id)
}

func (s *Service) CreateSession(ctx context.Context, input store.SessionCreate) (domain.Session, error) {
	session, err := s.store.CreateSession(ctx, input)
	if err != nil {
		return domain.Session{}, err
	}
	_, _ = s.store.CreateActivity(ctx, domain.Activity{Label: "session", Kind: "session.created", Message: fmt.Sprintf("Session snapshot %s created", session.Title)})
	return session, nil
}

func (s *Service) UpdateSession(ctx context.Context, snapshotID string, input store.SessionUpdate) (domain.Session, error) {
	return s.store.UpdateSession(ctx, snapshotID, input)
}

func (s *Service) DeleteSession(ctx context.Context, snapshotID string) error {
	return s.store.DeleteSession(ctx, snapshotID)
}

func (s *Service) CreateUser(ctx context.Context, input store.UserCreate) (domain.User, error) {
	return s.store.CreateUser(ctx, input)
}

func (s *Service) UpdateUser(ctx context.Context, id string, input store.UserUpdate) (domain.User, error) {
	return s.store.UpdateUser(ctx, id, input)
}

func (s *Service) CreateAPIKey(ctx context.Context, input store.APIKeyCreate) (domain.APIKeySecret, error) {
	return s.store.CreateAPIKey(ctx, input)
}
