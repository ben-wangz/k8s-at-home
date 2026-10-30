package app

import (
	"encoding/json"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type workAssociations struct {
	Authority  string `json:"authority,omitempty"`
	Repository string `json:"repository,omitempty"`
	Digest     string `json:"digest,omitempty"`
	ImageID    string `json:"imageID,omitempty"`
	TaskID     string `json:"taskID,omitempty"`
	IntentID   string `json:"intentID,omitempty"`
	TransferID string `json:"transferID,omitempty"`
	SessionID  string `json:"sessionID,omitempty"`
}

type backgroundJobView struct {
	workqueue.Job
	workAssociations
	Revision           string               `json:"revision"`
	SourceRevision     string               `json:"sourceRevision"`
	WaitReason         string               `json:"waitReason,omitempty"`
	SourceAgeSeconds   float64              `json:"sourceAgeSeconds"`
	EnqueuedAgeSeconds *float64             `json:"enqueuedAgeSeconds"`
	Checkpoint         workqueue.Checkpoint `json:"checkpoint"`
	RunnableAgeSeconds *float64             `json:"runnableAgeSeconds"`
	ResponsibleWorkURL string               `json:"responsibleWorkURL,omitempty"`
	Impact             string               `json:"impact"`
	ImpactObservedAt   time.Time            `json:"impactObservedAt"`
	StatusURL          string               `json:"statusURL"`
	EventsURL          string               `json:"eventsURL"`
	ErrorBlocking      bool                 `json:"errorBlocking"`
}

func diagnosticJob(job workqueue.Job) backgroundJobView {
	view := backgroundJobView{Job: job, Revision: workqueue.Revision(job), SourceRevision: workqueue.SourceRevision(job), Checkpoint: job.Checkpoint, StatusURL: "/api/v1/background/" + job.Kind + "/" + job.ID, EventsURL: "/api/v1/events?workID=" + job.ID, SourceAgeSeconds: max(0, time.Since(job.CreatedAt).Seconds()), WaitReason: job.WaitReason, Impact: workImpact(job.Kind), ImpactObservedAt: job.ProgressAt}
	if job.State == "done" {
		view.Impact = "target_revision_completed"
	}
	if job.Superseded {
		view.State, view.Impact = "superseded", "target_revision_superseded"
	} else if job.State == "pending" && job.WaitReason != "" {
		view.State = "waiting_dependency"
	}
	if job.ResponsibleWorkID != "" {
		view.ResponsibleWorkURL = "/api/v1/background/" + job.ResponsibleWorkKind + "/" + job.ResponsibleWorkID
	}
	if job.State == "pending" && job.WaitReason == "" && !job.RunnableAt.IsZero() {
		age := max(0, time.Since(job.RunnableAt).Seconds())
		view.RunnableAgeSeconds = &age
	}
	view.ErrorBlocking = job.State == "failed" || job.State == "retry_wait"
	if !job.EnqueuedAt.IsZero() {
		age := max(0, time.Since(job.EnqueuedAt).Seconds())
		view.EnqueuedAgeSeconds = &age
	}
	var source struct {
		Authority  string          `json:"authority"`
		Repository string          `json:"repository"`
		Digest     string          `json:"digest"`
		Session    string          `json:"session"`
		TaskID     string          `json:"taskID"`
		IntentID   string          `json:"intentID"`
		Identity   json.RawMessage `json:"identity"`
	}
	_ = json.Unmarshal(job.Payload, &source)
	var identity struct{ Authority, Repository, Reference, Digest string }
	if len(source.Identity) > 0 && source.Identity[0] == '{' {
		_ = json.Unmarshal(source.Identity, &identity)
	}
	view.Authority, view.Repository, view.Digest, view.TaskID, view.IntentID, view.SessionID = source.Authority, source.Repository, source.Digest, source.TaskID, source.IntentID, source.Session
	if identity.Authority != "" {
		view.Authority, view.Repository = identity.Authority, identity.Repository
	}
	if view.Digest == "" {
		view.Digest = identity.Digest
	}
	if view.Authority != "" && identity.Reference != "" {
		view.ImageID = inventory.Identity{Authority: view.Authority, Repository: view.Repository, Reference: identity.Reference}.ID()
	}
	if view.SessionID != "" {
		view.TransferID = inventory.ID("upstream_download", view.Authority, view.Repository, view.Digest, view.SessionID)
	}
	if job.Kind == "wake_root" {
		_ = json.Unmarshal(job.Payload, &view.TaskID)
	}
	if job.State == "retry_wait" {
		view.WaitReason = "retry_backoff"
	}
	if job.State == "failed" {
		view.WaitReason = "manual_intervention"
	}
	view.Payload, view.Progress = nil, nil
	view.DiagnosticOutbox = nil
	return view
}

func workImpact(kind string) string {
	switch kind {
	case "inventory", "execution":
		return "inventory_freshness"
	case "notify", "wake_root":
		return "dependent_work_notification"
	case "download_cleanup":
		return "temporary_storage_and_download_capacity"
	default:
		return "unknown"
	}
}

func matchesWork(view backgroundJobView, values url.Values) bool {
	fields := map[string]string{"state": view.State, "reason": view.WaitReason, "authority": view.Authority, "repository": view.Repository, "imageID": view.ImageID, "taskID": view.TaskID, "intentID": view.IntentID, "transferID": view.TransferID, "digest": view.Digest, "errorClass": view.ErrorClass, "retryable": strconv.FormatBool(view.State == "retry_wait")}
	for key, value := range fields {
		if values.Get(key) != "" && values.Get(key) != value {
			return false
		}
	}
	return true
}
