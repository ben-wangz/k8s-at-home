package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

type transferView struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	workAssociations
	StartedAt             time.Time        `json:"startedAt"`
	ProgressAt            time.Time        `json:"progressAt"`
	FinishedAt            time.Time        `json:"finishedAt"`
	ObservedAt            time.Time        `json:"observedAt"`
	TransferredBytes      *int64           `json:"transferredBytes"`
	TotalBytes            *int64           `json:"totalBytes"`
	TemporaryBytes        *int64           `json:"temporaryBytes"`
	ReservedBytes         *int64           `json:"reservedBytes"`
	TemporaryObservedAt   time.Time        `json:"temporaryObservedAt"`
	ReservationObservedAt time.Time        `json:"reservationObservedAt"`
	TransferState         string           `json:"transferState"`
	CacheCommitState      string           `json:"cacheCommitState"`
	CleanupState          string           `json:"cleanupState"`
	QuotaReleaseState     string           `json:"quotaReleaseState"`
	CleanupWorkID         string           `json:"cleanupWorkID,omitempty"`
	CacheVersion          string           `json:"cacheVersion,omitempty"`
	RetryCount            int              `json:"retryCount"`
	ErrorClass            string           `json:"errorClass,omitempty"`
	NextAttemptAt         time.Time        `json:"nextAttemptAt,omitempty"`
	EvidenceSource        string           `json:"evidenceSource"`
	StorageSnapshotID     string           `json:"storageSnapshotID,omitempty"`
	TemporaryReason       string           `json:"temporaryReason"`
	Attempt               *transferAttempt `json:"attempt,omitempty"`
}

func (s *Service) projectDiagnosticTransfer(ctx context.Context, key string, data json.RawMessage) error {
	view := transferView{ObservedAt: time.Now().UTC(), TransferState: "unknown", CacheCommitState: "unknown", CleanupState: "unknown", QuotaReleaseState: "unknown", EvidenceSource: "persisted source record"}
	switch {
	case strings.HasPrefix(key, diagnostics.Prefix+"upload-receipts/"):
		if json.Unmarshal(data, &view) != nil {
			return nil
		}
	case strings.HasPrefix(key, "downloads/v1/") && strings.HasSuffix(key, "/state.json"):
		var source struct {
			Authority   string
			Repository  string
			Digest      string
			Session     string
			Offset      int64
			TotalSize   int64
			Reservation int64
			CreatedAt   time.Time
			ProgressAt  time.Time
		}
		if json.Unmarshal(data, &source) != nil {
			return nil
		}
		view.Kind = "upstream_download"
		view.Authority, view.Repository, view.Digest, view.SessionID = source.Authority, source.Repository, source.Digest, source.Session
		view.StartedAt, view.ProgressAt, view.TransferredBytes, view.ReservedBytes = source.CreatedAt, source.ProgressAt, &source.Offset, &source.Reservation
		if source.TotalSize >= 0 {
			view.TotalBytes = &source.TotalSize
		}
		view.TransferState = "in_progress"
		if source.TotalSize >= 0 && source.TotalSize == source.Offset {
			view.TransferState = "received"
		}
	case strings.HasPrefix(key, workqueue.Prefix) && strings.Contains(key, "/download_cleanup/"):
		var job workqueue.Job
		var source downloadCleanupWork
		if json.Unmarshal(data, &job) != nil || json.Unmarshal(job.Payload, &source) != nil {
			return nil
		}
		view.Kind = "upstream_download"
		view.workAssociations = diagnosticJob(job).workAssociations
		view.TransferState, view.CacheCommitState, view.CleanupState = "complete", "committed", job.State
		view.CacheVersion, view.CleanupWorkID = source.CacheVersion, job.ID
		view.ProgressAt, view.RetryCount, view.ErrorClass, view.NextAttemptAt = job.ProgressAt, job.RetryCount, job.ErrorClass, job.NextAttemptAt
		view.QuotaReleaseState = "pending_verification"
		s.observeTransferReservation(ctx, &view)
		if job.State == "done" {
			view.CleanupState = "completed_or_superseded"
		}
	case strings.HasPrefix(key, "uploads/sessions/"):
		var source uploadSession
		if json.Unmarshal(data, &source) != nil {
			return nil
		}
		view = uploadTransferView(source)
	case strings.HasPrefix(key, "tasks/state/"):
		var task tasks.Task
		if json.Unmarshal(data, &task) != nil || task.Kind != tasks.BlobKind {
			return nil
		}
		view.Kind = "distribution_write"
		view.Authority, view.Repository, view.Digest, view.TaskID, view.IntentID = task.Authority, task.Repository, task.BlobDigest, task.ID, task.ID
		view.SessionID = task.RoundID
		if view.SessionID == "" {
			view.SessionID = task.ID
		}
		view.ProgressAt = task.LastProgressAt
		for _, event := range task.DiagnosticOutbox {
			if event.Result == string(tasks.Running) {
				view.StartedAt = event.OccurredAt
				break
			}
		}
		view.TotalBytes = &task.BlobSize
		view.TransferState = string(task.Status)
		view.RetryCount, view.ErrorClass, view.NextAttemptAt = task.RetryCount, task.ErrorClass, task.NextAttemptAt
		view.CacheVersion = task.CacheVersion
		if task.Status == tasks.Solidified {
			view.TransferredBytes = &task.BlobSize
			view.FinishedAt = task.UpdatedAt
		}
	default:
		return nil
	}
	if view.SessionID == "" && view.TaskID == "" {
		return nil
	}
	if view.Kind != "client_upload" {
		view.ID = inventory.ID(view.Kind, view.Authority, view.Repository, view.Digest, view.SessionID)
	}
	view.TransferID = view.ID
	err := inventory.Update(ctx, s.store, diagnostics.Prefix+"transfers/"+view.ID+".json", func(current *transferView) error {
		if current.ProgressAt.After(view.ProgressAt) {
			return nil
		}
		if view.StartedAt.IsZero() {
			view.StartedAt = current.StartedAt
		}
		if view.TransferredBytes == nil {
			view.TransferredBytes = current.TransferredBytes
		}
		if current.CacheCommitState == "committed" && view.CacheCommitState != "committed" {
			return nil
		}
		*current = view
		return nil
	})
	if err != nil {
		return err
	}
	if view.Kind == "upstream_download" {
		return inventory.Update(ctx, s.store, transferSessionKey(view.SessionID), func(current *transferView) error {
			if current.ProgressAt.After(view.ProgressAt) {
				return nil
			}
			*current = view
			return nil
		})
	}
	return nil
}

func transferSessionKey(session string) string {
	return diagnostics.Prefix + "transfer-sessions/" + diagnostics.ID(session) + ".json"
}
