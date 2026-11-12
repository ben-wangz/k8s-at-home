package app

import (
	"encoding/json"
	"registry-hub/internal/workqueue"
	"time"
)

type terminalCleanupView struct {
	CleanupID                string   `json:"cleanupID"`
	RoundID                  string   `json:"roundID"`
	Generation               uint64   `json:"generation"`
	Phase                    *string  `json:"phase"`
	Substep                  *int     `json:"substep"`
	Processed                *uint64  `json:"processed"`
	RootDemands              *uint64  `json:"rootDemands"`
	SharedDemands            *uint64  `json:"sharedDemands"`
	SessionOffset            *int     `json:"sessionOffset"`
	Yields                   *uint64  `json:"yields"`
	Coverage                 string   `json:"coverage"`
	CounterUnit              string   `json:"counterUnit"`
	CursorUnit               string   `json:"cursorUnit"`
	Outcome                  string   `json:"outcome,omitempty"`
	YieldReason              string   `json:"yieldReason,omitempty"`
	UnknownOperation         string   `json:"unknownOperation,omitempty"`
	Confirmation             string   `json:"confirmation,omitempty"`
	RecoveryID               string   `json:"recoveryID,omitempty"`
	DeadlineRemainingSeconds *float64 `json:"deadlineRemainingSeconds,omitempty"`
}

func terminalCleanupDiagnostic(job workqueue.Job) *terminalCleanupView {
	var work terminalCleanupWork
	_ = json.Unmarshal(job.Payload, &work)
	v := &terminalCleanupView{CleanupID: work.CleanupID, RoundID: work.RoundID, Generation: work.Generation, Coverage: "unavailable", CounterUnit: "state_machine_steps", CursorUnit: "current_traversal_position"}
	if summary, ok := readTerminalSummary(job); ok && job.State == "done" {
		v.Phase, v.Processed, v.Yields = &summary.Phase, &summary.ProcessedSteps, &summary.Yields
		v.Coverage, v.Outcome, v.Confirmation, v.RecoveryID = summary.Coverage, summary.Outcome, summary.Confirmation, summary.RecoveryID
		return v
	}
	var p terminalCleanupProgress
	var fields map[string]json.RawMessage
	if len(job.Progress) == 0 || json.Unmarshal(job.Progress, &p) != nil || json.Unmarshal(job.Progress, &fields) != nil || !cleanupFieldPresent(fields, "processed") || !cleanupFieldPresent(fields, "yields") {
		return v
	}
	v.Processed, v.Yields = &p.Processed, &p.Yields
	if p.State.Step != "" {
		v.Phase, v.Substep = &p.State.Step, &p.State.Half
	}
	var state map[string]json.RawMessage
	if json.Unmarshal(fields["state"], &state) == nil {
		if cleanupFieldPresent(state, "inner") {
			v.RootDemands = &p.State.Inner.Entities
		}
		if cleanupFieldPresent(state, "demandCursor") {
			v.SharedDemands = &p.State.DemandCursor.Entities
		}
		if cleanupFieldPresent(state, "sessionIndex") || p.State.SessionIDs != nil {
			v.SessionOffset = &p.State.SessionIndex
		}
	}
	v.Coverage, v.YieldReason, v.Confirmation, v.RecoveryID = "active_checkpoint", p.YieldReason, p.Confirmation, p.RecoveryID
	if p.Pending != nil {
		v.UnknownOperation = p.Pending.Operation
	}
	if job.State == "running" && !p.DeadlineAt.IsZero() {
		remaining := max(0, time.Until(p.DeadlineAt).Seconds())
		v.DeadlineRemainingSeconds = &remaining
	}
	return v
}
