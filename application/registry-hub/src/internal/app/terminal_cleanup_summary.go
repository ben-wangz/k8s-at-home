package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

type terminalCleanupSummary struct {
	SchemaVersion   int        `json:"schemaVersion"`
	CleanupID       string     `json:"cleanupID"`
	RoundID         string     `json:"roundID"`
	Generation      uint64     `json:"generation"`
	Phase           string     `json:"phase"`
	Outcome         string     `json:"outcome"`
	ProcessedSteps  uint64     `json:"processedSteps"`
	Yields          uint64     `json:"yields"`
	Confirmation    string     `json:"confirmation,omitempty"`
	RecoveryID      string     `json:"recoveryID,omitempty"`
	FirstExecutedAt *time.Time `json:"firstExecutedAt"`
	CompletedAt     *time.Time `json:"completedAt"`
	CounterUnit     string     `json:"counterUnit"`
	Coverage        string     `json:"coverage"`
}

func (p terminalCleanupProgress) CompletionSummary(job workqueue.Job) (json.RawMessage, error) {
	var work terminalCleanupWork
	if json.Unmarshal(job.Payload, &work) != nil || work.CleanupID == "" {
		return nil, storage.ErrCorruptObject
	}
	outcome := "completed"
	if job.Superseded {
		outcome = "superseded"
	} else if p.Confirmation == "terminal_state_reconciled" {
		outcome = "reconciled"
	}
	phase := p.State.Step
	if phase == "" && outcome != "completed" {
		phase = outcome
	}
	return json.Marshal(terminalCleanupSummary{SchemaVersion: 1, CleanupID: work.CleanupID, RoundID: work.RoundID, Generation: work.Generation, Phase: phase, Outcome: outcome, ProcessedSteps: p.Processed, Yields: p.Yields, Confirmation: p.Confirmation, RecoveryID: p.RecoveryID, FirstExecutedAt: cleanupTime(job.FirstAttemptAt), CompletedAt: cleanupTime(job.CompletedAt), CounterUnit: "state_machine_steps", Coverage: "persisted_cleanup_execution"})
}

func cleanupTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

func buildBackgroundFinalSummary(job workqueue.Job) (json.RawMessage, error) {
	if job.Kind != "terminal_cleanup" || len(job.Progress) == 0 || bytes.Equal(job.Progress, []byte("null")) {
		return nil, nil
	}
	var p terminalCleanupProgress
	var fields map[string]json.RawMessage
	if json.Unmarshal(job.Progress, &p) != nil || json.Unmarshal(job.Progress, &fields) != nil {
		return nil, storage.ErrCorruptObject
	}
	if !cleanupFieldPresent(fields, "processed") || !cleanupFieldPresent(fields, "yields") {
		return nil, nil
	}
	return p.CompletionSummary(job)
}

func readTerminalSummary(job workqueue.Job) (terminalCleanupSummary, bool) {
	var result terminalCleanupSummary
	if len(job.FinalSummary) == 0 || len(job.FinalSummary) > workqueue.MaxFinalSummaryBytes {
		return result, false
	}
	decoder := json.NewDecoder(bytes.NewReader(job.FinalSummary))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !errors.Is(decoder.Decode(new(any)), io.EOF) || result.SchemaVersion != 1 || result.CounterUnit != "state_machine_steps" || result.Coverage != "persisted_cleanup_execution" {
		return terminalCleanupSummary{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(job.FinalSummary, &fields) != nil || result.Phase == "" {
		return terminalCleanupSummary{}, false
	}
	for _, key := range []string{"processedSteps", "yields", "generation", "roundID"} {
		if !cleanupFieldPresent(fields, key) {
			return terminalCleanupSummary{}, false
		}
	}
	var work terminalCleanupWork
	if json.Unmarshal(job.Payload, &work) != nil || result.CleanupID != work.CleanupID || result.RoundID != work.RoundID || result.Generation != work.Generation || result.CleanupID == "" {
		return terminalCleanupSummary{}, false
	}
	if result.Outcome != "completed" && result.Outcome != "superseded" && result.Outcome != "reconciled" {
		return terminalCleanupSummary{}, false
	}
	return result, true
}

func cleanupFieldPresent(fields map[string]json.RawMessage, key string) bool {
	return len(fields[key]) > 0 && !bytes.Equal(bytes.TrimSpace(fields[key]), []byte("null"))
}
