package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func TestStatisticsResumeCursorAndAccumulatorTogether(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seed := metricSweepCheckpoint{Status: backgroundScanStatus{StartedAt: time.Now().UTC(), RunID: "original-round"}, Cursor: inventory.Cursor{Entries: []storage.Entry{{Key: "already-counted"}, {Key: "remaining"}}, Offset: 1, Entities: 1}, Accumulator: json.RawMessage(`{"pending":1}`), LastComplete: json.RawMessage(`{"solidified":4}`)}
	if err := inventory.Update(ctx, f.service.store, metricCheckpointKey("resume"), func(value *metricSweepCheckpoint) error { *value = seed; return nil }); err != nil {
		t.Fatal(err)
	}
	counts, cursor := map[string]int64{}, inventory.Cursor{}
	published := make(chan map[string]int64, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.runMetricSweep(ctx, "resume", &cursor, accumulatorFor(&counts), func(context.Context) (bool, error) {
			if cursor.Offset != 1 || counts["pending"] != 1 || counts["solidified"] != 0 {
				t.Errorf("partial accumulator was lost or mixed with previous round: cursor=%+v counts=%v", cursor, counts)
			}
			counts["pending"]++
			cursor.Advance()
			return true, nil
		}, func() {
			copy := map[string]int64{}
			for key, value := range counts {
				copy[key] = value
			}
			published <- copy
		}, func() { counts = map[string]int64{}; cursor = inventory.Cursor{} })
	}()
	for _, expected := range []map[string]int64{{"solidified": 4}, {"pending": 2}} {
		select {
		case actual := <-published:
			if len(actual) != 1 {
				t.Errorf("published=%v", actual)
			}
			for key, value := range expected {
				if actual[key] != value {
					t.Errorf("published=%v expected=%v", actual, expected)
				}
			}
		case <-time.After(3 * time.Second):
			t.Fatal("statistics did not resume")
		}
	}
	cancel()
	<-done
}

func TestStatisticsTaskTotalsRemainSerializable(t *testing.T) {
	totals := newTaskMetricTotals()
	totals.add(tasks.Task{Status: tasks.RetryWait, RetryCount: 3, Phase: "solidification", Kind: "blob", ErrorClass: "storage"})
	raw, err := json.Marshal(totals)
	if err != nil {
		t.Fatal(err)
	}
	restored := newTaskMetricTotals()
	if err := accumulatorFor(&restored).Restore(raw); err != nil {
		t.Fatal(err)
	}
	if restored.Counts[tasks.RetryWait] != 1 || restored.Runnable != 1 || restored.ErrorCounts["storage"] != 1 || restored.RetryCounts["blob\x00retry_wait"] != 3 {
		t.Fatalf("incomplete restored totals: %+v", restored)
	}
}

func TestQueueCheckpointPreservesPartialEnumeration(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	holder, err := lease.NewManager(f.service.store).Acquire(ctx, "queue-recovery-test", "first-owner", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state := queueSweepCheckpoint{Cursor: inventory.Cursor{Entries: []storage.Entry{{Key: "first"}, {Key: "second"}}, Offset: 1, Entities: 1}, Status: workqueue.NewStatus(time.Now().UTC())}
	state.Status.Counts["done"] = 1
	state.Status.Resumability = "durable_checkpoint"
	if err := f.service.saveQueueSweep(ctx, holder, "notify", state); err != nil {
		t.Fatal(err)
	}
	stored, _, err := inventory.Read[queueSweepCheckpoint](ctx, f.service.store, queueCheckpointKey("notify"))
	if err != nil || stored.Cursor.Offset != 1 || stored.Status.Counts["done"] != 1 {
		t.Fatalf("checkpoint=%+v err=%v", stored, err)
	}
	state.Cursor.Done = true
	if err := f.service.saveQueueSweep(ctx, holder, "notify", state); err != nil {
		t.Fatal(err)
	}
	stored, _, err = inventory.Read[queueSweepCheckpoint](ctx, f.service.store, queueCheckpointKey("notify"))
	if err != nil || stored.Cursor.Entities != 0 || stored.Status.Counts["done"] != 0 || stored.NextRound.IsZero() {
		t.Fatalf("next round=%+v err=%v", stored, err)
	}
	summary, _, err := inventory.Read[workqueue.Status](ctx, f.service.store, workqueue.Prefix+"status/notify.json")
	if err != nil || summary.Counts["done"] != 1 {
		t.Fatalf("published summary=%+v err=%v", summary, err)
	}
}

func TestScanCheckpointRejectsPreviousLeaseOwner(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	previous, err := manager.Acquire(ctx, "scan-owner-test", "previous", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, previous); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Acquire(ctx, "scan-owner-test", "current", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	state := maintenanceCheckpoint{Status: backgroundScanStatus{Fence: current.FencingToken}, Cursor: json.RawMessage(`"continuation"`)}
	if err := f.service.saveMaintenanceCheckpoint(ctx, current, "storage_orphans", state); err != nil {
		t.Fatal(err)
	}
	if err := f.service.saveMaintenanceCheckpoint(ctx, previous, "storage_orphans", maintenanceCheckpoint{}); err == nil {
		t.Fatal("previous owner replaced checkpoint")
	}
	stored, _, err := inventory.Read[maintenanceCheckpoint](ctx, f.service.store, maintenanceCheckpointKey("storage_orphans"))
	if err != nil || string(stored.Cursor) != `"continuation"` {
		t.Fatalf("checkpoint=%+v err=%v", stored, err)
	}
}
