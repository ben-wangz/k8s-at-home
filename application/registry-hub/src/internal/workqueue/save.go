package workqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/storage"
)

func (q Queue) save(ctx context.Context, job *Job) error {
	class := ""
	if job.State == "failed" || job.State == "retry_wait" {
		class = job.ErrorClass
	}
	event := diagnostics.Event{Component: "background", Operation: job.Kind, EntityID: job.ID, WorkID: job.ID, WorkKind: job.Kind, RunID: job.RunID, Stage: job.State, Result: job.State, Attempt: job.Fence, AttemptLayer: "background_unit", Retryable: job.State == "retry_wait", ErrorClass: class, NextAttemptAt: job.NextAttemptAt, RecoveredAt: job.RecoveredAt, TimeoutScope: job.TimeoutScope, AdmissionWaitSeconds: &job.AdmissionWaitSeconds, ExecutionDurationSeconds: &job.ExecutionDurationSeconds, ExecutionBudgetSeconds: &job.ExecutionBudgetSeconds, AttemptID: job.AttemptID, ParentAttemptID: job.ParentAttemptID, StartedAt: job.LastAttemptAt, FinishedAt: job.AttemptFinishedAt, SourceRevision: SourceRevision(*job), Reason: job.WaitReason}
	if job.State == "pending" && !job.AttemptFinishedAt.IsZero() {
		event.Result = "budget_yield"
		if job.WaitReason != "" {
			event.Result = "waiting_dependency"
		}
	}
	if job.Superseded {
		event.Result = "superseded"
	}
	diagnostics.Record(&job.DiagnosticOutbox, event)
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	entry, err := q.Store.Put(ctx, Key(job.Kind, job.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: job.Version})
	if errors.Is(err, storage.ErrUnknownCommit) {
		actual, readErr := q.Read(ctx, job.Kind, job.ID)
		encoded, _ := json.Marshal(actual)
		if readErr == nil && bytes.Equal(encoded, data) {
			job.Version = actual.Version
			return nil
		}
		return errors.Join(err, readErr)
	}
	if err == nil {
		job.Version = entry.Version
	}
	return err
}
