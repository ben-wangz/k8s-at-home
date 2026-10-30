package tasks

import (
	"testing"
	"time"
)

func TestTaskRecoverySurvivesRunningTransitionAndOutboxRotation(t *testing.T) {
	start := time.Now().UTC()
	task := Task{ID: "task", Status: RetryWait, ErrorClass: "storage", ErrorMessage: "storage call timed out; responsibility unknown", AttemptID: "first", UpdatedAt: start}
	recordTaskEvent(&task)
	task.Status, task.AttemptID, task.UpdatedAt = Running, "second", start.Add(time.Second)
	recordTaskEvent(&task)
	if !task.RecoveredAt.IsZero() || task.LastError == nil || !task.LastError.OccurredAt.Equal(start) {
		t.Fatal("starting an attempt erased or recovered the previous failure")
	}
	task.DiagnosticOutbox = nil
	task.Status, task.ErrorClass, task.ErrorMessage = Solidified, "", ""
	task.UpdatedAt = start.Add(2 * time.Second)
	recordTaskEvent(&task)
	if !task.RecoveredAt.Equal(task.UpdatedAt) || task.LastError.Class != "storage" || !task.DiagnosticOutbox[0].RecoveredAt.Equal(task.UpdatedAt) {
		t.Fatal("successful recovery lost retained error evidence")
	}
}

func TestIntentRecoveryDoesNotTreatReadyAsSuccess(t *testing.T) {
	start := time.Now().UTC()
	intent := BlobIntent{ID: "intent", State: IntentRetryWait, ErrorClass: "timeout", UpdatedAt: start}
	recordIntentEvent(&intent)
	intent.State, intent.UpdatedAt = IntentReady, start.Add(time.Second)
	recordIntentEvent(&intent)
	if !intent.RecoveredAt.IsZero() {
		t.Fatal("ready is not proof of successful verification")
	}
	intent.State, intent.UpdatedAt = IntentVerified, start.Add(2*time.Second)
	recordIntentEvent(&intent)
	if !intent.RecoveredAt.Equal(intent.UpdatedAt) || intent.LastError == nil {
		t.Fatal("verification failed to preserve the recovered error")
	}
}
