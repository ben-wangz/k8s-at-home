package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func TestTerminalCleanupRecoversJournalAfterCrash(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			f, faults, root := terminalFixture(t, 1)
			ctx := context.Background()
			root, err := f.service.tasks.Cancel(ctx, root.ID)
			if err != nil {
				t.Fatal(err)
			}
			q := workqueue.Queue{Store: f.service.store}
			job, err := q.Claim(ctx, terminalJob(t, f.service, root))
			if err != nil {
				t.Fatal(err)
			}
			demands, _, err := f.service.records.ListRootBlobDemandRecords(ctx, root.ID, "", 1)
			if err != nil || len(demands) != 1 {
				t.Fatal(err)
			}
			demand := demands[0]
			p := terminalCleanupProgress{State: protectionRecoveryCheckpoint{Step: "demand_remove", Demand: &demand, SessionIDs: root.SessionIDs}}
			if _, _, err := p.State.Inner.Peek(ctx, f.service.store, "solidification/blobs/root-owners/"+root.ID+"/", 16); err != nil {
				t.Fatal(err)
			}
			manager := lease.NewManager(f.service.store)
			held, err := manager.Acquire(ctx, "tasks/"+root.ID, "crashed-terminal", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			var work terminalCleanupWork
			if err := json.Unmarshal(job.Payload, &work); err != nil {
				t.Fatal(err)
			}
			scope := &terminalWriteScope{service: f.service, job: &job, work: work, progress: &p, before: p, parent: ctx, manager: manager, lease: held, epoch: f.service.gate.State().Epoch}
			key := "solidification/blobs/owners/" + demand.IntentID + "/" + demand.DemandID + ".json"
			entry, err := f.service.store.Head(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if err := scope.begin(ctx, terminalMutation{Operation: "delete", Key: key, Version: entry.Version}); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err := faults.Store.DeleteIfMatch(ctx, key, entry.Version); err != nil {
					t.Fatal(err)
				}
			}
			if err := manager.Release(ctx, held); err != nil {
				t.Fatal(err)
			}
			if err := inventory.Update(ctx, f.service.store, workqueue.Key(job.Kind, job.ID), func(current *workqueue.Job) error {
				current.LeaseUntil = time.Now().Add(-time.Second)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, _, err = terminalQuantum(t, f.service, root, time.Second)
			if committed {
				if err != nil || f.service.gate.State().RecoveryBarrier {
					t.Fatalf("confirmed crash replay entered recovery: %v", err)
				}
			} else {
				if !errors.Is(err, storage.ErrUnknownCommit) || !f.service.gate.State().RecoveryBarrier {
					t.Fatalf("unconfirmed journal was released: %v", err)
				}
				trigger := f.service.gate.State().RecoveryTrigger
				if trigger == nil || trigger.WorkID != job.ID || trigger.TaskID != root.ID || trigger.Kind != "terminal_cleanup" {
					t.Fatalf("recovery lost cleanup associations: %+v", trigger)
				}
				if err := f.service.recoverSolidificationProtection(ctx); err != nil {
					t.Fatal(err)
				}
			}
			drainBackgroundWork(t, f.service, "terminal_cleanup")
			if gate := f.service.gate.State(); gate.ReservedDistributionBytes != 0 || gate.ProtectionSessionCount != 0 || gate.RecoveryBarrier {
				t.Fatalf("journal recovery corrupted accounting: %+v", gate)
			}
		})
	}
}

func TestTerminalKnownWriteCheckpointFailureKeepsGateOpen(t *testing.T) {
	f, faults, root := terminalFixture(t, 1)
	ctx := context.Background()
	root, err := f.service.tasks.Cancel(ctx, root.ID)
	if err != nil {
		t.Fatal(err)
	}
	q := workqueue.Queue{Store: f.service.store}
	job, err := q.Claim(ctx, terminalJob(t, f.service, root))
	if err != nil {
		t.Fatal(err)
	}
	issued := false
	faults.deleteHook = func(key string) error {
		if strings.HasPrefix(key, "solidification/blobs/owners/") {
			issued = true
		}
		return nil
	}
	faults.putHook = func(key string, data []byte) error {
		var current workqueue.Job
		var p terminalCleanupProgress
		if strings.HasPrefix(key, "background/v1/jobs/terminal_cleanup/") && json.Unmarshal(data, &current) == nil && json.Unmarshal(current.Progress, &p) == nil && issued && p.Pending == nil {
			return storage.ErrPermissionDenied
		}
		return nil
	}
	progress, done, cause := f.service.terminalCleanupStep(ctx, &job)
	if !issued || done || !errors.Is(cause, errTerminalCheckpointPending) || f.service.gate.State().RecoveryBarrier {
		t.Fatalf("known result was escalated: issued=%t done=%t error=%v gate=%+v", issued, done, cause, f.service.gate.State())
	}
	faults.putHook, faults.deleteHook = nil, nil
	if err := q.Finish(ctx, &job, progress, done, cause); err != nil {
		t.Fatal(err)
	}
	view, err := json.Marshal(diagnosticJob(job))
	if err != nil || strings.Contains(string(view), "solidification/blobs/owners/") || !strings.Contains(string(view), "known_result_checkpoint_pending") {
		t.Fatalf("diagnostics leaked a journal or lost confirmation: %s %v", view, err)
	}
	drainBackgroundWork(t, f.service, "terminal_cleanup")
}
