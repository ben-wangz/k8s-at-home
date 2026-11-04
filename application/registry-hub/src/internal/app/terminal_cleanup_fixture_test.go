package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func terminalFixture(t *testing.T, demands int) (*stagedAcceptanceFixture, *recoveryFaultStore, tasks.Task) {
	t.Helper()
	f, faults := newRecoveryFixture(t)
	manager, err := tasks.NewManager(f.service.store, 1, 100, f.processor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	f.service.tasks, f.processor.tasks = manager, manager
	ctx := context.Background()
	intent := newReadyBlobIntent(t, ctx, manager, f.service.records)
	root := newDemandRootTask(t, ctx, manager, "terminal-budget")
	session := mustGateTestID(t)
	if err := f.service.gate.AdmitProtection(ctx, session, "terminal-scope", root.ID, 64, 10, 1024); err != nil {
		t.Fatal(err)
	}
	root.ScopeID, root.SessionIDs, root.GCProtected = "terminal-scope", []string{session}, true
	root.RequestedBytes, root.ReservedBytes = 64, 64
	held, err := manager.Leases().Acquire(ctx, "tasks/"+root.ID, "fixture", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.SaveProgress(ctx, &root, held); err != nil {
		t.Fatal(err)
	}
	if err := manager.Leases().Release(ctx, held); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < demands; n++ {
		linkRootBlobDemand(t, ctx, f.service.records, intent.ID, root.ID, fmt.Sprintf("%032x", n+1), root.ScopeID, session)
	}
	return f, faults, root
}

func terminalJob(t *testing.T, s *Service, task tasks.Task) workqueue.Job {
	t.Helper()
	id := workqueue.ID("terminal_cleanup", task.ID+"/"+task.TerminalCleanupID)
	job, err := (workqueue.Queue{Store: s.store}).Read(context.Background(), "terminal_cleanup", id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func terminalQuantum(t *testing.T, s *Service, task tasks.Task, budget time.Duration) (terminalCleanupProgress, bool, error) {
	t.Helper()
	q := workqueue.Queue{Store: s.store}
	job, err := q.Claim(context.Background(), terminalJob(t, s, task))
	if err != nil {
		t.Fatal(err)
	}
	parent := context.WithValue(context.Background(), backgroundParentKey{}, context.Background())
	ctx, cancel := context.WithTimeout(parent, budget)
	guarded := inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return q.Validate(ctx, job) })
	progress, done, cause := s.terminalCleanupStep(guarded, &job)
	cancel()
	if err := q.Finish(context.Background(), &job, progress, done, cause); err != nil {
		t.Fatal(err)
	}
	return progress.(terminalCleanupProgress), done, cause
}
