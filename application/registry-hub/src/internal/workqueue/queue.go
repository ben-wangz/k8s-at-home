package workqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const Prefix = "background/v1/"

var ErrSourceMissing = errors.New("background source is missing")

type Queue struct {
	Store               storage.Store
	ValidateRetrySource func(context.Context, Job) error
}

func Key(kind, id string) string { return Prefix + "jobs/" + kind + "/" + id + ".json" }

func ID(parts ...string) string {
	data, _ := json.Marshal(parts)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (q Queue) Enqueue(ctx context.Context, kind, source string, payload any, observed time.Time) (Job, error) {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 8<<20 || observed.IsZero() {
		return Job{}, errors.Join(storage.ErrCorruptObject, err)
	}
	now := time.Now().UTC()
	job := Job{SchemaVersion: 1, ID: ID(kind, source), Kind: kind, Payload: data, State: "pending", CreatedAt: observed, EnqueuedAt: now, ProgressAt: now, RunnableAt: now}
	diagnostics.Record(&job.DiagnosticOutbox, diagnostics.Event{Component: "background", Operation: kind, EntityID: job.ID, WorkID: job.ID, WorkKind: kind, Stage: "enqueue", Result: "pending", OccurredAt: now})
	if receipt, _, readErr := inventory.Read[Job](ctx, q.Store, ReceiptKey(kind, job.ID)); readErr == nil {
		if !bytes.Equal(receipt.Payload, data) {
			return Job{}, storage.ErrCorruptObject
		}
		return receipt, nil
	} else if !errors.Is(readErr, storage.ErrNotFound) {
		return Job{}, readErr
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		return Job{}, err
	}
	entry, err := q.Store.Put(ctx, Key(kind, job.ID), bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		job.Version = entry.Version
		return job, nil
	}
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		stored, readErr := q.Read(ctx, kind, job.ID)
		if readErr == nil && bytes.Equal(stored.Payload, data) {
			return stored, nil
		}
		if readErr == nil {
			return Job{}, storage.ErrCorruptObject
		}
		return Job{}, errors.Join(err, readErr)
	}
	return Job{}, err
}

func (q Queue) Read(ctx context.Context, kind, id string) (Job, error) {
	job, version, err := inventory.Read[Job](ctx, q.Store, Key(kind, id))
	if errors.Is(err, storage.ErrNotFound) {
		job, version, err = inventory.Read[Job](ctx, q.Store, ReceiptKey(kind, id))
	}
	if err == nil && (job.SchemaVersion != 1 || job.ID != id || job.Kind != kind || job.CreatedAt.IsZero()) {
		err = storage.ErrCorruptObject
	}
	job.Version = version
	return job, err
}

func (q Queue) Claim(ctx context.Context, job Job) (Job, error) {
	now := time.Now().UTC()
	if job.State == "done" || job.State == "failed" || job.NextAttemptAt.After(now) || job.State == "running" && job.LeaseUntil.After(now) {
		return Job{}, storage.ErrConditionFailed
	}
	if receipt, _, err := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID)); err == nil && receipt.State == "done" {
		_ = q.Store.DeleteIfMatch(ctx, Key(job.Kind, job.ID), job.Version)
		return Job{}, storage.ErrConditionFailed
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return Job{}, err
	}
	if job.State == "running" {
		diagnostics.Record(&job.DiagnosticOutbox, diagnostics.Event{Component: "background", Operation: job.Kind, EntityID: job.ID, WorkID: job.ID, WorkKind: job.Kind, Stage: "lease_takeover", Result: "recovered", AttemptID: job.AttemptID, RunID: job.RunID})
	}
	job.ParentAttemptID = job.AttemptID
	job.State, job.LeaseUntil, job.Fence = "running", now.Add(time.Minute), job.Fence+1
	job.AttemptID = ID("attempt", job.ID, now.Format(time.RFC3339Nano), job.RunID)
	job.LastAttemptAt, job.AttemptFinishedAt = now, time.Time{}
	if job.FirstAttemptAt.IsZero() {
		job.FirstAttemptAt = now
	}
	job.WaitReason, job.ResponsibleWorkID, job.ResponsibleWorkKind = "", "", ""
	err := q.save(ctx, &job)
	return job, err
}

func (q Queue) Validate(ctx context.Context, job Job) error {
	current, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		return err
	}
	if current.Fence != job.Fence || current.State != "running" || !current.LeaseUntil.After(time.Now()) {
		return storage.ErrLeaseLost
	}
	return nil
}

func (q Queue) Finish(ctx context.Context, job *Job, progress any, done bool, cause error) error {
	if err := q.Validate(ctx, *job); err != nil {
		return err
	}
	data, err := json.Marshal(progress)
	if err != nil {
		return err
	}
	if !bytes.Equal(job.Progress, data) || done {
		job.ProgressAt = time.Now().UTC()
	}
	job.AttemptFinishedAt = time.Now().UTC()
	job.Progress, job.State, job.LeaseUntil = data, "pending", time.Time{}
	job.Checkpoint = checkpointSummary(data)
	var checkpoint struct {
		Phase int `json:"phase"`
	}
	if json.Unmarshal(data, &checkpoint) == nil {
		job.Phase = checkpoint.Phase
	}
	job.NextAttemptAt = time.Time{}
	if done {
		job.State = "done"
		job.CompletedAt = job.AttemptFinishedAt
		if !job.ErrorAt.IsZero() {
			job.RecoveredAt = time.Now().UTC()
		}
	}
	if cause != nil {
		job.RetryCount++
		job.State, job.ErrorClass, job.ErrorAt = "retry_wait", "storage", time.Now().UTC()
		if errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, storage.ErrTimeout) {
			job.ErrorClass = "timeout"
		}
		if errors.Is(cause, context.Canceled) {
			job.ErrorClass = "cancelled"
		}
		job.ErrorPhase = job.Phase
		if errors.Is(cause, storage.ErrCorruptObject) {
			job.State, job.ErrorClass = "failed", "corrupt_source"
		}
		if errors.Is(cause, ErrSourceMissing) {
			job.State, job.ErrorClass = "failed", "source_missing"
		}
		delay := min(time.Second*time.Duration(1<<min(job.RetryCount-1, 8)), 5*time.Minute)
		job.NextAttemptAt = time.Now().UTC().Add(delay + time.Duration(time.Now().UnixNano()%int64(delay/4+1)))
		job.RunnableAt = job.NextAttemptAt
	} else if !done {
		job.RunnableAt = job.AttemptFinishedAt
	}
	if err := q.save(ctx, job); err != nil {
		return err
	}
	if job.State == "done" {
		return q.Archive(ctx, *job)
	}
	return nil
}

func ReceiptKey(kind, id string) string { return Prefix + "receipts/" + kind + "/" + id + ".json" }

func (q Queue) Archive(ctx context.Context, job Job) error {
	job.Progress = nil
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = q.Store.Put(ctx, ReceiptKey(job.Kind, job.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		stored, _, readErr := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID))
		if readErr != nil || !bytes.Equal(stored.Payload, job.Payload) {
			return errors.Join(err, readErr)
		}
		err = nil
	}
	if err != nil {
		return err
	}
	err = q.Store.DeleteIfMatch(ctx, Key(job.Kind, job.ID), job.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (q Queue) Retry(ctx context.Context, kind, id string) error {
	job, err := q.Read(ctx, kind, id)
	if err != nil {
		return err
	}
	if err := q.finalizeRetryMarker(ctx, job); err != nil {
		return err
	}
	if job.State != "failed" && job.State != "retry_wait" {
		return storage.ErrConditionFailed
	}
	if q.ValidateRetrySource != nil {
		if err := q.ValidateRetrySource(ctx, job); err != nil {
			return err
		}
	}
	job.State, job.NextAttemptAt, job.RunnableAt = "pending", time.Time{}, time.Now().UTC()
	return q.save(ctx, &job)
}
