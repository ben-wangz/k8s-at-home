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
	Phase                    string   `json:"phase"`
	Substep                  int      `json:"substep"`
	Processed                uint64   `json:"processed"`
	RootDemands              uint64   `json:"rootDemands"`
	SharedDemands            uint64   `json:"sharedDemands"`
	SessionOffset            int      `json:"sessionOffset"`
	Yields                   uint64   `json:"yields"`
	YieldReason              string   `json:"yieldReason,omitempty"`
	UnknownOperation         string   `json:"unknownOperation,omitempty"`
	Confirmation             string   `json:"confirmation,omitempty"`
	RecoveryID               string   `json:"recoveryID,omitempty"`
	DeadlineRemainingSeconds *float64 `json:"deadlineRemainingSeconds,omitempty"`
}

func terminalCleanupDiagnostic(job workqueue.Job) *terminalCleanupView {
	var work terminalCleanupWork
	var p terminalCleanupProgress
	_ = json.Unmarshal(job.Payload, &work)
	_ = json.Unmarshal(job.Progress, &p)
	v := &terminalCleanupView{CleanupID: work.CleanupID, RoundID: work.RoundID, Generation: work.Generation, Phase: p.State.Step, Substep: p.State.Half, Processed: p.Processed, RootDemands: p.State.Inner.Entities, SharedDemands: p.State.DemandCursor.Entities, SessionOffset: p.State.SessionIndex, Yields: p.Yields, YieldReason: p.YieldReason, Confirmation: p.Confirmation, RecoveryID: p.RecoveryID}
	if p.Pending != nil {
		v.UnknownOperation = p.Pending.Operation
	}
	if job.State == "running" && !p.DeadlineAt.IsZero() {
		remaining := max(0, time.Until(p.DeadlineAt).Seconds())
		v.DeadlineRemainingSeconds = &remaining
	}
	return v
}
