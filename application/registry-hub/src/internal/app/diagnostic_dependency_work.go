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
	view.IntentState = string(intent.State)
	view.TaskURL = "/api/v1/tasks/" + intent.ID
	view.ActivationWorkerURL = "/api/v1/workers/activation"
	view.NotificationCoverage = "current intent revision with no excluded root; other notification variants may exist"
	view.NotificationsURL = "/api/v1/blob-intents/" + intent.ID + "/notifications"
	view.NextAttemptAt = intent.NextAttemptAt
	view.LastProgressAt = intent.LastProgressAt
	var linkedTask *tasks.Task
	if task, err := s.tasks.Get(ctx, intent.ID); err == nil {
		view.TaskObservation, view.TaskID, view.TaskStatus = "found", task.ID, string(task.Status)
		view.TaskUpdatedAt, view.AttemptStartedAt = task.UpdatedAt, task.AttemptStartedAt
		view.TaskNextAttemptAt, view.TaskLastProgressAt = task.NextAttemptAt, task.LastProgressAt
		view.TaskWaitReason, view.WaitResourceKey = task.WaitReason, task.WaitResourceKey
		if taskMatchesIntent(task, intent) {
			linkedTask = &task
		} else {
			view.TaskObservation = "inconsistent"
			view.ReadErrors["task"] = "task_intent_mismatch"
		}
	} else if errors.Is(err, storage.ErrNotFound) {
		view.TaskObservation = "absent"
	} else {
		view.TaskObservation = "unreadable"
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
	if work != nil && work.State != "done" && work.State != "superseded" && (intent.State == tasks.IntentVerified || intent.State == tasks.IntentAnchored) {
		view.Reason, view.NextAction = "notification_pending", "wait_dependency_notification"
		return
	}
	if linkedTask != nil {
		if reason, action, ok := taskDependencyWait(*linkedTask); ok {
			view.Reason, view.NextAction = reason, action
			return
		}
	}
	view.Reason, view.NextAction = string(intent.State), intentNextAction(intent.State)
	if intent.State == tasks.IntentWaitingCache && view.CacheState == "complete" {
		view.Reason, view.NextAction = "activation_pending", "wait_blob_activation"
	}
}

func taskMatchesIntent(task tasks.Task, intent tasks.BlobIntent) bool {
	return task.ID == intent.ID && (task.Kind == tasks.BlobKind || task.Kind == "") &&
		task.Authority == intent.Identity.Authority && task.Repository == intent.Identity.Repository &&
		task.BlobDigest == intent.Identity.Digest && task.Generation == intent.Generation &&
		(intent.CacheVersion == "" || task.CacheVersion == "" || task.CacheVersion == intent.CacheVersion) &&
		(intent.ActualSize == 0 || task.BlobSize == 0 || task.BlobSize == intent.ActualSize)
}

func taskDependencyWait(task tasks.Task) (string, string, bool) {
	switch task.WaitReason {
	case "object_lease_pending":
		return "waiting_object_lease", "wait_object_lease", true
	case "protection_busy", "protection_reconcile_pending", "protection_write_pending", "protection_transfer":
		return "waiting_protection_reconciliation", "wait_protection_reconciliation", true
	case "distribution_quota":
		return "waiting_distribution_capacity", "wait_capacity", true
	case "protected_cache_capacity":
		return "waiting_protected_cache_capacity", "wait_capacity", true
	case "blob_intent_admission":
		return "waiting_blob_intent_admission", "wait_blob_intent_cleanup", true
	}
	switch task.Status {
	case tasks.WaitingQuota:
		return "waiting_capacity", "wait_capacity", true
	case tasks.RetryWait:
		return "task_retry_waiting", "wait_automatic_retry", true
	case tasks.Running:
		return "task_running", "wait_task_progress", true
	case tasks.VerifiedLocal:
		return "awaiting_manifest_anchor", "wait_manifest_anchor", true
	}
	return "", "", false
}
