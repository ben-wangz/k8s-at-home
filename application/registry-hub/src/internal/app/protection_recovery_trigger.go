package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"time"
)

type recoveryTrigger struct {
	Kind         string    `json:"kind"`
	Reason       string    `json:"reason"`
	TaskID       string    `json:"taskID,omitempty"`
	WorkID       string    `json:"workID,omitempty"`
	AttemptID    string    `json:"attemptID,omitempty"`
	TriggeredAt  time.Time `json:"triggeredAt,omitempty"`
	ErrorClass   string    `json:"errorClass,omitempty"`
	ErrorSummary string    `json:"errorSummary,omitempty"`
}

type recoveryTriggerKey struct{}

func withRecoveryTrigger(ctx context.Context, kind, reason, task, work, attempt string, cause error) context.Context {
	trigger := &recoveryTrigger{Kind: kind, Reason: reason, TaskID: task, WorkID: work, AttemptID: attempt, TriggeredAt: time.Now().UTC()}
	if cause != nil {
		trigger.ErrorClass, trigger.ErrorSummary = diagnosticReason(cause), diagnostics.SafeErrorText(cause)
	}
	return context.WithValue(ctx, recoveryTriggerKey{}, trigger)
}

func barrierTrigger(ctx context.Context, reason string) *recoveryTrigger {
	if trigger, ok := ctx.Value(recoveryTriggerKey{}).(*recoveryTrigger); ok {
		return trigger
	}
	kind := "runtime_protection"
	if reason == "startup_reconcile" {
		kind = "startup"
	}
	return &recoveryTrigger{Kind: kind, Reason: reason, TriggeredAt: time.Now().UTC()}
}
