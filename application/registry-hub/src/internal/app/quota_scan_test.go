package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestQuotaScanBatchRestartRetainsCandidatesAndExplicitEnd(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 64)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for n := 0; n < 37; n++ {
		task := tasks.Task{ID: fmt.Sprintf("%032x", n+1), Status: tasks.WaitingQuota, WaitReason: "distribution_quota", QuotaWaitAt: base.Add(time.Duration(n) * time.Second)}
		if err := inventory.Update(ctx, f.service.store, fmt.Sprintf("scan-test/%032x.json", n+1), func(v *tasks.Task) error { *v = task; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	p := sourceSweepCheckpoint{Round: 1, Backoff: map[string]entityBackoff{}}
	step := func(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
		task, _, err := inventory.Read[tasks.Task](ctx, f.service.store, entry.Key)
		if err != nil {
			return err
		}
		if p.QuotaCandidates == nil {
			p.QuotaCandidates = map[string]tasks.Task{}
		}
		selectQuotaCandidate(p.QuotaCandidates, task)
		return nil
	}
	f.service.sourceSweepBatch(ctx, "quota_waiters", "scan-test/", &p, step)
	if p.Cursor.Entities != 16 || p.Cursor.Done || p.QuotaCandidates["distribution_quota"].ID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("unbounded/incomplete batch: %+v", p)
	}
	holder, err := lease.NewManager(f.service.store).Acquire(ctx, "source-test", "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p.Status.Fence = holder.FencingToken
	if err = f.service.saveSourceSweep(ctx, holder, "quota-test", p); err != nil {
		t.Fatal(err)
	}
	p, _, err = inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey("quota-test"))
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 2; n++ {
		f.service.sourceSweepBatch(ctx, "quota_waiters", "scan-test/", &p, step)
	}
	if !p.Cursor.Done || p.Cursor.Entities != 37 || p.QuotaCandidates["distribution_quota"].ID != fmt.Sprintf("%032x", 1) {
		t.Fatalf("restart lost candidates: %+v", p)
	}
	// End-of-prefix handling must not wait for a first record in the next round.
	p.QuotaCandidates = map[string]tasks.Task{}
	if err = f.service.completeQuotaScan(ctx, &p); err != nil {
		t.Fatal(err)
	}
	f.service.completeSourceRound("quota_waiters", &p)
	if p.Round != 2 || p.Status.LastScanCompletedAt.IsZero() || !p.Status.Complete {
		t.Fatalf("round not completed: %+v", p)
	}
	assertMetricsCounter(t, f.service.metrics, "registry_hub_background_scan_rounds_total", map[string]string{"kind": "quota_waiters", "result": "complete"}, 1)
	empty := sourceSweepCheckpoint{Round: 1, Backoff: map[string]entityBackoff{}}
	f.service.sourceSweepBatch(ctx, "quota_waiters", "empty-prefix/", &empty, step)
	if !empty.Cursor.Done {
		t.Fatal("empty prefix not completed")
	}
	if err = f.service.completeQuotaScan(ctx, &empty); err != nil {
		t.Fatal(err)
	}
}

func TestSourceBudgetYieldDoesNotAdvanceUnprocessedEntity(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 64)
	ctx, cancel := context.WithCancel(context.Background())
	p := sourceSweepCheckpoint{Round: 1, Backoff: map[string]entityBackoff{}, Cursor: inventory.Cursor{Entries: []storage.Entry{{Key: "unprocessed", Version: "one"}}}}
	f.service.sourceSweepEntity(ctx, "quota_waiters", "ignored/", &p, func(context.Context, storage.Entry, *sourceSweepCheckpoint) error { cancel(); return context.Canceled })
	if p.Cursor.Entities != 0 || p.Cursor.Offset != 0 || p.RoundFailed || len(p.Backoff) != 0 {
		t.Fatalf("read budget treated as processed failure: %+v", p)
	}
	if pureContextBudget(errors.Join(context.Canceled, storage.ErrUnknownCommit)) {
		t.Fatal("unknown mutation treated as budget yield")
	}
}

func TestQuotaCheckpointFenceRejectsPreviousOwner(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 64)
	ctx := context.Background()
	manager := lease.NewManager(f.service.store)
	old, err := manager.Acquire(ctx, "quota-source", "old", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = manager.Release(ctx, old); err != nil {
		t.Fatal(err)
	}
	current, err := manager.Acquire(ctx, "quota-source", "new", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := sourceSweepCheckpoint{Status: backgroundScanStatus{Fence: current.FencingToken}, QuotaWakeIndex: 1, QuotaCandidates: map[string]tasks.Task{"distribution_quota": {ID: "candidate"}}}
	if err = f.service.saveSourceSweep(ctx, current, "quota_waiters", p); err != nil {
		t.Fatal(err)
	}
	if err = f.service.saveSourceSweep(ctx, old, "quota_waiters", sourceSweepCheckpoint{}); err == nil {
		t.Fatal("old fence published candidates")
	}
	stored, _, err := inventory.Read[sourceSweepCheckpoint](ctx, f.service.store, sourceSweepKey("quota_waiters"))
	data, _ := json.Marshal(stored)
	if err != nil || stored.QuotaWakeIndex != 1 || stored.QuotaCandidates["distribution_quota"].ID != "candidate" {
		t.Fatalf("checkpoint lost: %s %v", data, err)
	}
}

func TestSourceBatchesPreserveNonQuotaAdmissionGranularity(t *testing.T) {
	for _, kind := range []string{"source_downloads", "activation", "task_reconciliation"} {
		t.Run(kind, func(t *testing.T) {
			p := sourceSweepCheckpoint{Cursor: inventory.Cursor{Entries: make([]storage.Entry, 20)}}
			processed := 0
			service := &Service{}
			service.sourceSweepBatch(context.Background(), kind, "unused/", &p, func(context.Context, storage.Entry, *sourceSweepCheckpoint) error {
				processed++
				return nil
			})
			if processed != 1 || p.Cursor.Entities != 1 {
				t.Fatalf("unrelated source held admission across %d entities: %+v", processed, p.Cursor)
			}
		})
	}
}
