package workqueue

import (
	"context"
	"encoding/json"
	"registry-hub/internal/storage"
	"time"
)

// Checkpoint retains the running claim and its fencing token.
func (q Queue) Checkpoint(ctx context.Context, job *Job, progress any) error {
	if err := q.Validate(ctx, *job); err != nil {
		return err
	}
	data, err := json.Marshal(progress)
	if err != nil || len(data) > 8<<20 {
		return storage.ErrCorruptObject
	}
	job.Progress, job.ProgressAt = data, time.Now().UTC()
	job.Checkpoint = checkpointSummary(data)
	return q.save(ctx, job)
}
