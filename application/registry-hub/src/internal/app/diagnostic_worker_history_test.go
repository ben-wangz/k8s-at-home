package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestProtectionRecoveryHistorySurvivesExecutionTimeoutAndRestart(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	parent := context.Background()
	ctx, cancel := context.WithCancel(context.WithValue(parent, backgroundParentKey{}, parent))
	finish := f.service.observeProtectionRecovery(ctx)
	cancel()
	finish(context.DeadlineExceeded)
	key := diagnostics.Prefix + "workers/protection_recovery.json"
	failed, _, err := inventory.Read[recoveryHistory](parent, f.service.store, key)
	if err != nil || failed.LastError == nil || failed.LastError.Class != "timeout" || len(failed.DiagnosticOutbox) != 1 {
		t.Fatalf("failure was lost after execution expired: %+v %v", failed, err)
	}
	f.service.workerStates = nil
	finish = f.service.observeProtectionRecovery(parent)
	finish(nil)
	recovered, _, err := inventory.Read[recoveryHistory](parent, f.service.store, key)
	if err != nil || recovered.LastError == nil || !recovered.LastError.OccurredAt.Equal(failed.LastError.OccurredAt) || recovered.RecoveredAt.Before(failed.LastError.OccurredAt) || len(recovered.DiagnosticOutbox) != 2 {
		t.Fatalf("recovery erased prior failure: %+v %v", recovered, err)
	}
}

func TestSourceSweepRestoresFailureAndPersistsRecovery(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := "test-sources/one.json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *int) error { *value = 1; return nil }); err != nil {
		t.Fatal(err)
	}
	entry, err := f.service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := lease.NewManager(f.service.store).Acquire(ctx, "source-recovery-test", "replacement-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	status := backgroundScanStatus{ErrorEntity: key, ErrorAt: time.Now().Add(-time.Minute), RunID: "original-round"}
	seed := sourceSweepCheckpoint{Status: status, Round: 1, Cursor: inventory.Cursor{Entries: []storage.Entry{entry}}, Backoff: map[string]entityBackoff{key + "\x00" + entry.Version: {Retries: 1, Next: time.Now().Add(-time.Second), Round: 1}}}
	f.service.recordScanFailure(context.WithValue(ctx, diagnosticContextKey{}, backgroundDiagnosticContext{RunID: "failed-run"}), "source_tasks", "source_entity", &seed.Status, &seed.DiagnosticOutbox, storage.ErrTimeout, 1)
	if err := f.service.saveSourceSweep(ctx, holder, "source_tasks", seed); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.runSourceSweepLeader(ctx, holder, "source_tasks", "test-sources/", func(context.Context, storage.Entry) error { return nil })
	}()
	defer func() { cancel(); <-done }()
	var recovered sourceSweepCheckpoint
	for ctx.Err() == nil {
		recovered, _, err = inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey("source_tasks"))
		if err == nil && recovered.Status.LastError != nil && !recovered.Status.RecoveredAt.Before(recovered.Status.LastError.OccurredAt) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || recovered.Status.LastError == nil || recovered.Status.RecoveredAt.IsZero() || len(recovered.DiagnosticOutbox) != 2 {
		t.Fatalf("restarted source lost error or recovery: %+v %v", recovered, err)
	}
	if !recovered.Status.LastError.OccurredAt.Equal(seed.Status.LastError.OccurredAt) || recovered.DiagnosticOutbox[1].ParentAttemptID != "failed-run" {
		t.Fatal("source recovery replaced the original failure identity")
	}
	if err := f.service.collectDiagnosticOutbox(ctx, sourceSweepKey("source_tasks"), recovered.DiagnosticOutbox); err != nil {
		t.Fatal(err)
	}
	if err := f.service.flushDiagnosticPendingPartition(ctx, diagnostics.EventBatchDate(recovered.DiagnosticOutbox[0]), true); err != nil {
		t.Fatalf("flush recovered source events: %v", err)
	}
	for _, event := range recovered.DiagnosticOutbox {
		if _, err := f.service.store.Head(ctx, diagnostics.EventKey(event)); err == nil {
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("read checkpointed legacy event: %v", err)
		}
		entries, _, err := storage.Scan(ctx, f.service.store, diagnostics.BatchPrefix, "", 32)
		if err != nil {
			t.Fatalf("list checkpointed event batches: %v", err)
		}
		collected := false
		for _, entry := range entries {
			batch, _, readErr := inventory.Read[diagnostics.EventBatch](ctx, f.service.store, entry.Key)
			if readErr != nil {
				t.Fatalf("read checkpointed event batch: %v", readErr)
			}
			if diagnostics.VerifyEventBatch(batch, entry.Key) != nil {
				t.Fatalf("checkpointed event batch is invalid: %s", entry.Key)
			}
			for _, archived := range batch.Events {
				if archived.ID == event.ID {
					collected = true
					break
				}
			}
			if collected {
				break
			}
		}
		if !collected {
			t.Fatalf("checkpointed event %s was not collected as a legacy object or batch event", event.ID)
		}
	}
}

func TestWorkerBudgetsDescribeIndependentRunners(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	for _, row := range []struct {
		kind    string
		shared  int
		seconds float64
	}{{"source_tasks", 2, 15}, {"statistics_tasks", 2, 10}, {"storage_probe", 0, 15}, {"diagnostic_events", 0, 5}, {"index_repair", 0, 30}, {"protection_recovery", 0, 60}} {
		view := workerView{workerStatus: workerStatus{ID: row.kind}}
		f.service.configureWorkerBudgets(&view)
		if view.SharedConcurrency != row.shared || view.ExecutionBudgetSeconds != row.seconds {
			t.Fatalf("incorrect budgets for %s: %+v", row.kind, view)
		}
	}
	f.service.cfg.Inventory.BatchTimeout = 37 * time.Second
	view := workerView{workerStatus: workerStatus{ID: "inventory_builder"}}
	f.service.configureWorkerBudgets(&view)
	if view.ExecutionBudgetSeconds != 37 || view.SharedConcurrency != 0 || view.CheckpointBudgetSeconds != 0 {
		t.Fatalf("inventory builder inherited unrelated runner budgets: %+v", view)
	}
}

func TestOlderRecoveryCannotClearAnotherInstancesNewFailure(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.observeProtectionRecovery(ctx)(storage.ErrTimeout)
	olderSuccess := f.service.observeProtectionRecovery(ctx)
	other := &Service{store: f.service.store}
	other.observeProtectionRecovery(ctx)(storage.ErrUnavailable)
	key := diagnostics.Prefix + "workers/protection_recovery.json"
	before, _, err := inventory.Read[recoveryHistory](ctx, f.service.store, key)
	if err != nil || before.LastError == nil {
		t.Fatalf("new failure missing: %+v %v", before, err)
	}
	olderSuccess(nil)
	after, _, err := inventory.Read[recoveryHistory](ctx, f.service.store, key)
	if err != nil || after.LastError == nil || after.LastError.AttemptID != before.LastError.AttemptID || !after.RecoveredAt.Before(after.LastError.OccurredAt) {
		t.Fatalf("older recovery cleared another instance's failure: %+v %v", after, err)
	}
}
