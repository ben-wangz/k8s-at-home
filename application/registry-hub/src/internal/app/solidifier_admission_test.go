package app

import (
	"context"
	"errors"
	"testing"

	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
)

func TestSolidifierAdmissionUsesSharedSlotAndProcessDoesNotReacquire(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	backgroundRelease, ok := f.service.acquireSolidificationSlot(ctx, false)
	if !ok {
		t.Fatal("could not reserve shared solidification slot")
	}
	if _, _, ok := f.processor.AdmitTask(ctx, &tasks.Task{}); ok {
		t.Fatal("task admission succeeded while the shared slot was full")
	}
	backgroundRelease()

	admittedCtx, release, ok := f.processor.AdmitTask(ctx, &tasks.Task{})
	if !ok {
		t.Fatal("task admission failed after the shared slot was released")
	}
	defer release()
	if active := len(f.service.solidificationSlots); active != 1 {
		t.Fatalf("admission reserved %d slots, want exactly one", active)
	}
	err := f.processor.Process(admittedCtx, &tasks.Task{ID: "invalid-blob", Kind: tasks.BlobKind}, lease.Lease{})
	var failure *tasks.Failure
	if !errors.As(err, &failure) || failure.Class != "task_state" {
		t.Fatalf("processor result = %v, want task_state after admission", err)
	}
	if active := len(f.service.solidificationSlots); active != 1 {
		t.Fatalf("processor changed admitted slot count to %d", active)
	}
}
