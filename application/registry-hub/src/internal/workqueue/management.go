package workqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

type RetryRequest struct {
	ExpectedRevision string `json:"expectedRevision"`
	SourceRevision   string `json:"sourceRevision"`
	IdempotencyKey   string `json:"idempotencyKey"`
}

type RetryAcceptance struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	WorkID     string       `json:"workID"`
	Request    RetryRequest `json:"request"`
	AcceptedAt time.Time    `json:"acceptedAt,omitempty"`
	Revision   string       `json:"revision,omitempty"`
	StatusURL  string       `json:"statusURL"`
}

func Revision(job Job) string       { return ID("revision", job.Version) }
func SourceRevision(job Job) string { return ID("source", string(job.Payload)) }

func (q Queue) AcceptRetry(ctx context.Context, principal, kind, id string, request RetryRequest) (RetryAcceptance, error) {
	if request.ExpectedRevision == "" || request.SourceRevision == "" || len(request.IdempotencyKey) < 8 || len(request.IdempotencyKey) > 128 {
		return RetryAcceptance{}, inventory.ErrInvalid
	}
	acceptance := RetryAcceptance{ID: ID("retry", principal, request.IdempotencyKey), Kind: kind, WorkID: id, Request: request, StatusURL: "/api/v1/background/" + kind + "/" + id}
	key := Prefix + "retry-requests/" + acceptance.ID + ".json"
	data, _ := json.Marshal(acceptance)
	_, err := q.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err != nil && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return acceptance, err
	}
	stored, version, err := inventory.Read[RetryAcceptance](ctx, q.Store, key)
	if err != nil {
		return acceptance, err
	}
	if stored.Kind != kind || stored.WorkID != id || stored.Request != request {
		return acceptance, storage.ErrConditionFailed
	}
	if !stored.AcceptedAt.IsZero() {
		return stored, nil
	}
	job, err := q.Read(ctx, kind, id)
	if err != nil {
		return acceptance, err
	}
	if job.RetryRequestID != acceptance.ID {
		if err := q.finalizeRetryMarker(ctx, job); err != nil {
			return acceptance, err
		}
		if Revision(job) != request.ExpectedRevision || SourceRevision(job) != request.SourceRevision || (job.State != "failed" && job.State != "retry_wait") {
			return acceptance, storage.ErrConditionFailed
		}
		if q.ValidateRetrySource != nil {
			if err := q.ValidateRetrySource(ctx, job); err != nil {
				return acceptance, err
			}
		}
		job.State, job.NextAttemptAt, job.RetryRequestID = "pending", time.Time{}, acceptance.ID
		job.RunnableAt = time.Now().UTC()
		job.RetryAcceptedAt = time.Now().UTC()
		diagnostics.Record(&job.DiagnosticOutbox, diagnostics.Event{Component: "background", Operation: kind, EntityID: job.ID, WorkID: job.ID, WorkKind: kind, RunID: acceptance.ID, Stage: "manual_retry", Result: "accepted", Attempt: job.Fence, AttemptLayer: "management_request"})
		if err := q.save(ctx, &job); err != nil {
			return acceptance, err
		}
	}
	stored.AcceptedAt, stored.Revision = job.RetryAcceptedAt, Revision(job)
	if stored.AcceptedAt.IsZero() {
		stored.AcceptedAt = time.Now().UTC()
	}
	data, _ = json.Marshal(stored)
	_, err = q.Store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version})
	if err != nil {
		actual, _, readErr := inventory.Read[RetryAcceptance](ctx, q.Store, key)
		if readErr == nil && actual.Request == request && !actual.AcceptedAt.IsZero() {
			return actual, nil
		}
	}
	return stored, err
}

func (q Queue) finalizeRetryMarker(ctx context.Context, job Job) error {
	if job.RetryRequestID == "" {
		return nil
	}
	return inventory.Update(ctx, q.Store, Prefix+"retry-requests/"+job.RetryRequestID+".json", func(prior *RetryAcceptance) error {
		if prior.ID != job.RetryRequestID {
			return storage.ErrCorruptObject
		}
		if prior.AcceptedAt.IsZero() {
			prior.AcceptedAt, prior.Revision = job.RetryAcceptedAt, Revision(job)
			if prior.AcceptedAt.IsZero() {
				prior.AcceptedAt = job.ProgressAt
			}
		}
		return nil
	})
}
