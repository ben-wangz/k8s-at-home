package tasks

import (
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"strconv"
)

func recordTaskEvent(task *Task) {
	digest := task.RootDigest
	if digest == "" {
		digest = task.BlobDigest
	}
	class := task.ErrorClass
	if task.Status != Failed && task.Status != RetryWait && task.Status != RepairPending {
		class = ""
	}
	imageID := ""
	if task.Reference != "" {
		imageID = (inventory.Identity{Authority: task.Authority, Repository: task.Repository, Reference: task.Reference}).ID()
	}
	summary := ""
	if class != "" {
		summary = task.ErrorMessage
	}
	diagnostics.ObserveFailure(&task.LastError, &task.RecoveredAt, class, summary, task.Phase, task.AttemptID, task.UpdatedAt, task.Status == Solidified)
	diagnostics.Record(&task.DiagnosticOutbox, diagnostics.Event{Component: "solidification", Operation: string(task.Kind), EntityID: task.ID, ImageID: imageID, TaskID: task.ID, Authority: task.Authority, Repository: task.Repository, Digest: digest, OccurredAt: task.UpdatedAt, Stage: task.Phase, Result: string(task.Status), Attempt: task.FencingToken, AttemptLayer: "solidification_task", Retryable: task.Retryable, ErrorClass: class, ErrorSummary: summary, Reason: task.WaitReason, NextAttemptAt: task.NextAttemptAt, AttemptID: task.AttemptID, ParentAttemptID: task.ParentAttemptID, StartedAt: task.AttemptStartedAt, FinishedAt: task.AttemptFinishedAt, RecoveredAt: task.RecoveredAt})
}

func recordIntentEvent(intent *BlobIntent) {
	class := ""
	if intent.State == IntentRetryWait || intent.State == IntentFailed {
		class = intent.ErrorClass
	}
	attemptID := diagnostics.ID("blob_activation", intent.ID, strconv.FormatUint(intent.Round, 10), strconv.Itoa(intent.RetryCount))
	summary := ""
	if class != "" {
		summary = "blob activation failed; see error class and retry schedule"
	}
	diagnostics.ObserveFailure(&intent.LastError, &intent.RecoveredAt, class, summary, string(intent.State), attemptID, intent.UpdatedAt, intent.State == IntentVerified || intent.State == IntentAnchored)
	diagnostics.Record(&intent.DiagnosticOutbox, diagnostics.Event{Component: "blob_activation", Operation: "intent", EntityID: intent.ID, IntentID: intent.ID, Authority: intent.Identity.Authority, Repository: intent.Identity.Repository, Digest: intent.Identity.Digest, OccurredAt: intent.UpdatedAt, Stage: string(intent.State), Result: string(intent.State), Attempt: uint64(intent.RetryCount), AttemptLayer: "blob_activation", AttemptID: attemptID, Retryable: intent.State == IntentRetryWait, ErrorClass: class, ErrorSummary: summary, NextAttemptAt: intent.NextAttemptAt, RecoveredAt: intent.RecoveredAt})
}
