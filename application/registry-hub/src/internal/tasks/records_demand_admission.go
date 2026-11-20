package tasks

import (
	"context"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

var (
	ErrDemandAdmissionClosed  = errors.New("blob demand admission is closed")
	ErrDemandAdmissionWaiting = errors.New("blob demand admission is waiting for cleanup")
)

func demandAdmissionError(outcome error) error {
	return fmt.Errorf("%w: %w", outcome, storage.ErrConditionFailed)
}

func (r *Records) validateDemandAdmission(ctx context.Context, intentID string) error {
	intent, err := r.GetBlobIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if intent.State == IntentFailed || intent.State == IntentCancelled {
		return demandAdmissionError(ErrDemandAdmissionClosed)
	}
	if intent.CancelRequested {
		return demandAdmissionError(ErrDemandAdmissionWaiting)
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
	if task.Status == RepairPending || task.CancelRequested && task.Status != Cancelled && task.Status != Failed {
		return demandAdmissionError(ErrDemandAdmissionWaiting)
	}
	if task.Status == Cancelled || task.Status == Failed || task.Status == Solidified {
		if task.Status == RepairPending || task.Status == Cancelled && !task.TerminalCleanupComplete ||
			task.TerminalCleanupID != "" && !task.TerminalCleanupComplete || task.GCProtected || len(task.SessionIDs) > 0 ||
			task.ProtectionSessionID != "" || task.RecoveryReason != "" {
			return demandAdmissionError(ErrDemandAdmissionWaiting)
		}
		return demandAdmissionError(ErrDemandAdmissionClosed)
	}
	return nil
}

func (r *Records) ValidateBlobIntentCleanup(ctx context.Context, intentID string) error {
	intent, err := r.GetBlobIntent(ctx, intentID)
	if err != nil {
		return err
	}
	if (intent.State == IntentFailed || intent.State == IntentCancelled) && intent.ProtectionSessionID != "" {
		return demandAdmissionError(ErrDemandAdmissionWaiting)
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
	if task.Status != Cancelled && task.Status != Failed && task.Status != Solidified ||
		task.Status == Cancelled && !task.TerminalCleanupComplete ||
		task.TerminalCleanupID != "" && !task.TerminalCleanupComplete || task.GCProtected || len(task.SessionIDs) > 0 ||
		task.ProtectionSessionID != "" || task.RecoveryReason != "" {
		return demandAdmissionError(ErrDemandAdmissionWaiting)
	}
	return nil
}
