package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (r *Records) validateDemandAdmission(ctx context.Context, intentID string) error {
	intent, err := r.GetBlobIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if intent.State == IntentFailed || intent.State == IntentCancelled {
		return storage.ErrConditionFailed
	}
	task, _, err := inventory.Read[Task](ctx, r.store, taskKey(intentID))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if task.ID != intentID || task.SchemaVersion < taskSchemaLegacy || task.SchemaVersion > taskSchemaCurrent {
		return storage.ErrCorruptObject
	}
	if task.CancelRequested || task.Status == Cancelled || task.Status == Failed || task.Status == RepairPending {
		return storage.ErrConditionFailed
	}
	return nil
}
