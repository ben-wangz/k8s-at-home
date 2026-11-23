package workqueue

import (
	"context"
	"time"

	"registry-hub/internal/storage"
)

func ReceiptKey(kind, id string) string { return Prefix + "receipts/" + kind + "/" + id + ".json" }

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
	if err := q.save(ctx, &job); err != nil {
		return err
	}
	q.notifyRunnable(job)
	return nil
}

func (q Queue) notifyRunnable(job Job) {
	if q.OnRunnable != nil && job.State != "done" && job.State != "failed" {
		q.OnRunnable(job.Kind)
	}
}
