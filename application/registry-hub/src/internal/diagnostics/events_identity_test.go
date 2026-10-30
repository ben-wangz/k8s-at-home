package diagnostics

import "testing"

func TestOutboxKeepsDistinctEntitiesAndAttempts(t *testing.T) {
	var outbox []Event
	event := Event{Component: "background", Operation: "scan", EntityID: "first", RunID: "run-1", AttemptID: "attempt-1", Stage: "source_entity", Result: "failed", Attempt: 1, ErrorClass: "timeout"}
	Record(&outbox, event)
	Record(&outbox, event)
	if len(outbox) != 1 {
		t.Fatal("identical source transition was not coalesced")
	}
	event.EntityID = "second"
	Record(&outbox, event)
	event.AttemptID = "attempt-2"
	Record(&outbox, event)
	if len(outbox) != 3 {
		t.Fatal("different entities or attempts were silently dropped")
	}
	event.EntityID, event.Result, event.ErrorClass = "third", "done", ""
	Record(&outbox, event)
	if !outbox[3].RecoveredAt.IsZero() {
		t.Fatal("another entity's success was reported as recovery")
	}
	if SafeErrorText(nil) != "" {
		t.Fatal("successful operation has an error summary")
	}
}
