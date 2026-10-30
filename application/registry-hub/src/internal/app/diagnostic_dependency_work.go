package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type responsibleWork struct {
	ID            string               `json:"id"`
	Kind          string               `json:"kind"`
	State         string               `json:"state"`
	StatusURL     string               `json:"statusURL"`
	ProgressAt    time.Time            `json:"progressAt"`
	NextAttemptAt time.Time            `json:"nextAttemptAt"`
	ErrorClass    string               `json:"errorClass,omitempty"`
	Checkpoint    workqueue.Checkpoint `json:"checkpoint"`
}

func (s *Service) intentNotification(ctx context.Context, intent tasks.BlobIntent) (*responsibleWork, error) {
	id := workqueue.ID("notify", notificationSource(intent, ""))
	job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "notify", id)
	if err != nil {
		return nil, err
	}
	return notificationWork(job), nil
}

func notificationWork(job workqueue.Job) *responsibleWork {
	view := diagnosticJob(job)
	return &responsibleWork{ID: job.ID, Kind: job.Kind, State: view.State, StatusURL: view.StatusURL, ProgressAt: job.ProgressAt, NextAttemptAt: job.NextAttemptAt, ErrorClass: job.ErrorClass, Checkpoint: view.Checkpoint}
}

func (s *Service) enrichIntentDependency(ctx context.Context, intent tasks.BlobIntent, view *dependencyView) {
	view.IntentID = intent.ID
	view.IntentURL = "/api/v1/blob-intents/" + intent.ID
	view.ActivationWorkerURL = "/api/v1/workers/activation"
	view.NotificationCoverage = "current intent revision with no excluded root; other notification variants may exist"
	view.NotificationsURL = "/api/v1/blob-intents/" + intent.ID + "/notifications"
	view.NextAttemptAt = intent.NextAttemptAt
	view.LastProgressAt = intent.LastProgressAt
	if task, err := s.tasks.Get(ctx, intent.ID); err == nil {
		view.TaskID = task.ID
	} else if !errors.Is(err, storage.ErrNotFound) {
		view.ReadErrors["task"] = diagnosticReason(err)
	}
	work, err := s.intentNotification(ctx, intent)
	if err == nil {
		view.NotificationWork = work
	} else if !errors.Is(err, storage.ErrNotFound) {
		view.ReadErrors["notification"] = diagnosticReason(err)
	}
	if view.RegistryState == "verified" {
		return
	}
	view.Reason, view.NextAction = string(intent.State), intentNextAction(intent.State)
	if intent.State == tasks.IntentWaitingCache && view.CacheState == "complete" {
		view.Reason, view.NextAction = "activation_pending", "wait_blob_activation"
	}
	if work != nil && work.State != "done" && work.State != "superseded" && (intent.State == tasks.IntentVerified || intent.State == tasks.IntentAnchored) {
		view.Reason, view.NextAction = "notification_pending", "wait_dependency_notification"
	}
}
