package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/inventory"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestTerminalSummarySurvivesArchiveAndRestart(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1024)
	q := workqueue.Queue{Store: f.service.store, BuildFinalSummary: buildBackgroundFinalSummary}
	ctx := context.Background()
	w := terminalCleanupWork{TaskID: "task", CleanupID: "cleanup", RoundID: "round", Generation: 3}
	job, err := q.Enqueue(ctx, "terminal_cleanup", "summary", w, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	p := terminalCleanupProgress{Processed: 83, Yields: 6, Confirmation: "confirmed"}
	p.State.Step, p.State.Entity = "done", "private-storage-key"
	p.State.Inner.Entities = 45
	if err = q.Finish(ctx, &job, p, true, nil); err != nil {
		t.Fatal(err)
	}
	q = workqueue.Queue{Store: f.service.store}
	receipt, err := q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipt.Progress) != 0 {
		t.Fatal("receipt retained progress")
	}
	view := diagnosticJob(receipt)
	v := view.TerminalCleanup
	if v.Phase == nil || *v.Phase != "done" || v.Processed == nil || *v.Processed != 83 || v.Yields == nil || *v.Yields != 6 || v.RootDemands != nil || v.Coverage != "persisted_cleanup_execution" {
		t.Fatalf("summary lost: %+v", v)
	}
	if view.FinalSummary == nil || view.FinalSummary.FirstExecutedAt.IsZero() || view.FinalSummary.CompletedAt.IsZero() {
		t.Fatalf("missing summary timing: %+v", view.FinalSummary)
	}
	encoded, _ := json.Marshal(view)
	for _, private := range []string{"private-storage-key", "demandCursor", "validationLedger", "pending", "progress\""} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private execution data exposed: %s", encoded)
		}
	}
	var fields map[string]any
	if json.Unmarshal(encoded, &fields) != nil || fields["payload"] != nil || fields["progress"] != nil {
		t.Fatal("private job bodies exposed")
	}
	if err = q.Archive(ctx, job); err != nil {
		t.Fatal(err)
	}
	again, _ := q.Read(ctx, job.Kind, job.ID)
	if string(again.FinalSummary) != string(receipt.FinalSummary) {
		t.Fatal("duplicate archive changed summary")
	}
}

func TestTerminalSummaryLegacyAndInvalidEvidenceIsUnknown(t *testing.T) {
	w, _ := json.Marshal(terminalCleanupWork{CleanupID: "cleanup", RoundID: "round", Generation: 1})
	for _, data := range []string{"", `{}`, `{"schemaVersion":2}`, strings.Repeat("x", workqueue.MaxFinalSummaryBytes+1)} {
		job := workqueue.Job{Kind: "terminal_cleanup", State: "done", Payload: w, FinalSummary: json.RawMessage(data)}
		v := diagnosticJob(job)
		if v.TerminalCleanup.Processed != nil || v.TerminalCleanup.Yields != nil || v.TerminalCleanup.Phase != nil || v.FinalSummary != nil || v.TerminalCleanup.Coverage != "unavailable" {
			t.Fatalf("invented evidence: %+v", v)
		}
	}
	for _, progress := range []string{`{}`, `{"processed":null,"yields":0}`, `{"processed":0}`, `{"processed":1,"yields":null}`} {
		job := workqueue.Job{Kind: "terminal_cleanup", State: "pending", Payload: w, Progress: json.RawMessage(progress)}
		v := terminalCleanupDiagnostic(job)
		if v.Processed != nil || v.Yields != nil || v.Phase != nil || v.Coverage != "unavailable" {
			t.Fatalf("invented active counters: %+v", v)
		}
	}
}

func TestTerminalSummaryUpgradesMatchingLegacyReceipt(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1024)
	q := workqueue.Queue{Store: f.service.store, BuildFinalSummary: buildBackgroundFinalSummary}
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "terminal_cleanup", "legacy", terminalCleanupWork{CleanupID: "cleanup", RoundID: "round", Generation: 2}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job.State, job.Fence, job.CompletedAt = "done", 1, time.Now().UTC()
	job.Progress, _ = json.Marshal(terminalCleanupProgress{Processed: 19, Yields: 2, State: protectionRecoveryCheckpoint{Step: "done"}})
	if err = inventory.Update(ctx, q.Store, workqueue.Key(job.Kind, job.ID), func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
		t.Fatal(err)
	}
	legacy := job
	legacy.Progress = nil
	if err = inventory.Update(ctx, q.Store, workqueue.ReceiptKey(job.Kind, job.ID), func(value *workqueue.Job) error { *value = legacy; return nil }); err != nil {
		t.Fatal(err)
	}
	job, err = q.Read(ctx, job.Kind, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Archive(ctx, job); err != nil {
		t.Fatal(err)
	}
	receipt, _ := q.Read(ctx, job.Kind, job.ID)
	if summary, ok := readTerminalSummary(receipt); !ok || summary.ProcessedSteps != 19 || summary.Yields != 2 {
		t.Fatalf("legacy evidence not upgraded: %+v", receipt)
	}
}
