package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

func (s *Service) inspectReclaimEvidence(ctx context.Context, entry storage.Entry, object *storageObject) {
	if object.Cleaner != "" {
		object.CleanerURL = "/api/v1/workers/" + object.Cleaner
	}
	if object.Domain == "registry" {
		if object.Category == "registry_uploads" {
			object.CleanerURL = ""
			object.ReclaimReason = "distribution_upload_purge_policy_and_session_liveness_not_observed"
			return
		}
		object.CleanerURL = "/api/v1/gc"
		if s.gate == nil {
			return
		}
		gate := s.gate.State()
		if gate.RecoveryBarrier || gate.ProtectionSessionCount > 0 || gate.State != "open" {
			object.ProtectionState, object.ReclaimState, object.ReclaimReason = "gc_gate", "blocked", "gc_gate_or_solidification_protection_blocks_collection"
		}
		return
	}
	if object.Category == "download_temporary" && object.WorkID != "" {
		s.attachCleanupWork(ctx, object)
		return
	}
	if strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/") {
		job, _, err := inventory.Read[workqueue.Job](ctx, s.store, entry.Key)
		if err != nil {
			object.ReclaimReason = "receipt_" + diagnosticReason(err)
			return
		}
		object.Cleaner, object.CleanerURL = "receipt_cleanup", "/api/v1/workers/receipt_cleanup"
		if job.Kind == "execution" {
			object.ReclaimState, object.ReclaimReason = "blocked", "durable_execution_evidence_has_no_display_history_ttl"
			return
		}
		if !job.ProgressAt.IsZero() && time.Since(job.ProgressAt) > 30*24*time.Hour {
			object.ReclaimState, object.ReclaimReason = "candidate", "receipt_retention_elapsed; cleaner_revalidates_version"
		} else {
			object.ReclaimState, object.ReclaimReason = "blocked", "receipt_retention_not_elapsed"
		}
		return
	}
	if object.Category == "diagnostic_events" {
		event, _, err := inventory.Read[diagnostics.Event](ctx, s.store, entry.Key)
		if err != nil {
			object.ReclaimReason = "event_" + diagnosticReason(err)
			return
		}
		object.Cleaner, object.CleanerURL = "diagnostic_retention", "/api/v1/workers/diagnostic_retention"
		object.ReclaimState, object.ReclaimReason = "blocked", "event_retention_not_elapsed"
		if !event.OccurredAt.IsZero() && time.Since(event.OccurredAt) > diagnostics.Retention {
			object.ReclaimState, object.ReclaimReason = "candidate", "event_retention_elapsed; cleaner_revalidates_version"
		}
		return
	}
	if object.Category == "upload_sessions" {
		s.inspectUploadReclamation(ctx, entry.Key, object)
	}
}

func (s *Service) attachCleanupWork(ctx context.Context, object *storageObject) {
	job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "download_cleanup", object.WorkID)
	if err != nil {
		object.ReclaimReason = "cleanup_work_" + diagnosticReason(err)
		return
	}
	object.Cleaner, object.CleanerURL, object.WorkURL = "download_cleanup", "/api/v1/workers/download_cleanup", "/api/v1/background/download_cleanup/"+job.ID
	object.LastProgressAt, object.NextAttemptAt = job.ProgressAt, job.NextAttemptAt
	object.CleanupState = diagnosticJob(job).State
	switch job.State {
	case "pending":
		object.ReclaimState, object.ReclaimReason = "queued", "guarded_cleanup_enqueued"
	case "running":
		object.ReclaimState, object.ReclaimReason = "running", "guarded_cleanup_running"
	case "retry_wait":
		object.ReclaimState, object.ReclaimReason = "retry_wait", "cleanup_retry_"+stableDiagnosticError(job.ErrorClass)
	case "failed":
		object.ReclaimState, object.ReclaimReason = "manual_intervention", "cleanup_failed_"+stableDiagnosticError(job.ErrorClass)
	case "done":
		object.ReclaimState, object.ReclaimReason = "unknown", "completed_or_superseded_cleanup_does_not_prove_this_observed_object_was_removed"
	}
}

func (s *Service) inspectUploadReclamation(ctx context.Context, key string, object *storageObject) {
	state, _, err := inventory.Read[uploadSession](ctx, s.store, key)
	if err != nil {
		object.ReclaimReason = "upload_" + diagnosticReason(err)
		return
	}
	object.Cleaner, object.CleanerURL = "upload_cleanup", "/api/v1/workers/upload_cleanup"
	active, err := lease.NewManager(s.store).Active(ctx, "uploads/"+state.ID)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		object.ReclaimReason = "upload_lease_" + diagnosticReason(err)
		return
	}
	if active {
		object.ProtectionState, object.ReclaimState, object.ReclaimReason = "active_upload", "blocked", "upload_lease_held"
		return
	}
	object.ReclaimState, object.ReclaimReason = "blocked", "upload_idle_and_lifetime_deadlines_not_elapsed"
	if s.cfg.Limits.UploadIdleTimeout > 0 && time.Since(state.UpdatedAt) > s.cfg.Limits.UploadIdleTimeout || s.cfg.Limits.UploadMaxLifetime > 0 && time.Since(state.CreatedAt) > s.cfg.Limits.UploadMaxLifetime {
		object.ReclaimState, object.ReclaimReason = "candidate", "expired_upload; cleanup_must_cancel_remote_session_before_removing_metadata"
	}
}
