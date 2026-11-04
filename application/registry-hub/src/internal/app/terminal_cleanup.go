package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type terminalCleanupWork struct {
	TaskID     string      `json:"taskID"`
	CleanupID  string      `json:"cleanupID"`
	RoundID    string      `json:"roundID"`
	Generation uint64      `json:"generation"`
	Status     tasks.State `json:"status"`
	Authority  string      `json:"authority"`
	Repository string      `json:"repository"`
	Digest     string      `json:"digest"`
}

type terminalCleanupProgress struct {
	State        protectionRecoveryCheckpoint `json:"state"`
	Revision     uint64                       `json:"demandRevision"`
	Pending      *terminalMutation            `json:"pending,omitempty"`
	Processed    uint64                       `json:"processed"`
	Yields       uint64                       `json:"yields"`
	YieldReason  string                       `json:"yieldReason,omitempty"`
	Confirmation string                       `json:"confirmation,omitempty"`
	RecoveryID   string                       `json:"recoveryID,omitempty"`
	QuotaWaiters map[string]string            `json:"quotaWaiters,omitempty"`
	QuotaIndex   int                          `json:"quotaIndex,omitempty"`
	DeadlineAt   time.Time                    `json:"deadlineAt,omitempty"`
}

func (w terminalCleanupWork) matches(task tasks.Task) bool {
	return task.ID == w.TaskID && task.TerminalCleanupID == w.CleanupID && w.CleanupID != "" && task.RoundID == w.RoundID && task.Generation == w.Generation && task.Status == w.Status && isTerminalTask(task.Status)
}

func (s *Service) terminalCleanupStep(ctx context.Context, job *workqueue.Job) (result any, done bool, cause error) {
	defer func() {
		progress, ok := result.(terminalCleanupProgress)
		budget := errors.Is(cause, errTerminalBudgetYield) || ctx.Err() != nil && (errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, context.Canceled))
		if ok && budget && progress.Pending == nil && !errors.Is(cause, storage.ErrUnknownCommit) && !errors.Is(cause, storage.ErrLeaseLost) {
			progress.Yields++
			progress.YieldReason = "execution_budget"
			result, cause = progress, nil
		}
	}()
	var work terminalCleanupWork
	var progress terminalCleanupProgress
	if json.Unmarshal(job.Payload, &work) != nil || work.CleanupID == "" || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &progress) != nil {
		return progress, false, storage.ErrCorruptObject
	}
	progress.DeadlineAt, _ = ctx.Deadline()
	task, err := s.tasks.Get(ctx, work.TaskID)
	if errors.Is(err, storage.ErrNotFound) || err == nil && (!work.matches(task) || task.TerminalCleanupComplete) {
		job.Superseded = err != nil || !work.matches(task)
		if err == nil && work.matches(task) && task.TerminalCleanupComplete {
			progress.Pending, progress.Confirmation = nil, "terminal_state_reconciled"
		}
		return progress, true, nil
	}
	if err != nil {
		return progress, false, err
	}
	parent := ctx
	if lifecycle, ok := ctx.Value(backgroundParentKey{}).(context.Context); ok {
		parent = lifecycle
	}
	manager := lease.NewManager(s.store)
	leaseCtx, held, release, err := s.acquireBackgroundUnitLease(parent, "terminal_cleanup", manager, "tasks/"+task.ID, requestID(), time.Minute)
	if err != nil {
		if errors.Is(err, lease.ErrHeld) {
			progress.YieldReason = "task_lease_held"
			return progress, false, nil
		}
		return progress, false, err
	}
	defer release()
	releaseGate, err := s.gate.Enter(ctx)
	if err != nil {
		return progress, false, err
	}
	defer releaseGate()
	scope := &terminalWriteScope{service: s, job: job, work: work, progress: &progress, parent: leaseCtx, manager: manager, lease: held, epoch: s.gate.State().Epoch}
	if deadline, ok := ctx.Deadline(); ok {
		scope.reserve = min(2*time.Second, max(time.Millisecond, time.Until(deadline)/4))
	}
	guarded := context.WithValue(ctx, terminalWriteScopeKey{}, scope)
	if err := scope.guard(guarded); err != nil {
		return progress, false, err
	}
	if progress.Pending != nil {
		if err := scope.resume(); err != nil {
			if scope.confirmed {
				return progress, false, err
			}
			return progress, false, scope.recover(err)
		}
	}
	r := &protectionRecovery{service: s, checkpoint: progress.State}
	if r.checkpoint.Step == "" {
		r.checkpoint.Step, r.checkpoint.SessionIDs = "owners", s.gate.recoverySnapshot().SessionIDs
	}
	for unit := 0; unit < 16 && guarded.Err() == nil; unit++ {
		scope.before = progress
		err = s.advanceTerminalCleanup(guarded, r, task, &progress)
		if err != nil {
			break
		}
		progress.State, progress.Processed, progress.YieldReason = r.checkpoint, progress.Processed+1, "bounded_step"
		scope.before = progress
		if err = scope.journal(nil); err != nil || r.checkpoint.Step == "done" {
			break
		}
	}
	if progress.Pending != nil {
		if scope.confirmed {
			return progress, false, errors.Join(errTerminalCheckpointPending, err, scope.checkpointError)
		}
		return progress, false, scope.recover(err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) && ctx.Err() != nil || err == nil && ctx.Err() != nil {
		progress.Yields++
		progress.YieldReason = "execution_budget"
		return progress, false, nil
	}
	if errors.Is(err, ErrProtectionBusy) {
		progress.YieldReason = "remote_operation_pending"
		return progress, false, nil
	}
	if errors.Is(err, errTerminalRemoteUnknown) {
		return progress, false, scope.recover(err)
	}
	if err == nil && r.checkpoint.Step != "done" {
		progress.Yields++
		progress.YieldReason = "step_budget"
	}
	return progress, r.checkpoint.Step == "done", err
}

func (s *Service) advanceTerminalCleanup(ctx context.Context, r *protectionRecovery, task tasks.Task, p *terminalCleanupProgress) error {
	c := &r.checkpoint
	switch c.Step {
	case "owners":
		return r.taskOwnerStep(ctx, task)
	case "terminal_demands", "demand_remove", "demand_delete":
		if err := r.terminalDemandStep(ctx, task); err != nil {
			return err
		}
		if c.Step == "demand_check" {
			c.Step = "demand_revision"
		}
	case "demand_revision", "demand_check", "demand_cancel":
		return s.terminalSharedDemandStep(ctx, r, p)
	case "demand_enqueue":
		blob, err := s.tasks.Get(ctx, c.Demand.IntentID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && isTerminalTask(blob.Status) {
			processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
			if err := processor.TaskTerminal(ctx, &blob); err != nil {
				return err
			}
		}
		c.Step = "demand_delete"
	case "terminal_blob":
		return r.terminalBlobStep(ctx, task)
	case "terminal_sessions":
		if err := r.terminalSessionStep(ctx, task); err != nil {
			return err
		}
		if c.Step == "terminal_finish" {
			c.Step = "terminal_notify"
		}
	case "terminal_notify":
		if task.Kind == tasks.BlobKind {
			if err := s.notifyBlobDemandRoots(ctx, task.ID); err != nil {
				return err
			}
		}
		c.Step, c.Cursor = "terminal_quota_scan", inventory.Cursor{}
	case "terminal_quota_scan", "terminal_quota_wake":
		return s.terminalQuotaStep(ctx, c, p)
	case "terminal_finish":
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		processor.clearTaskProtection(&task)
		if err := s.tasks.SaveTerminalProtection(ctx, &task, false, ""); err != nil {
			return err
		}
		c.Step = "done"
	default:
		return storage.ErrCorruptObject
	}
	return nil
}

func (s *Service) terminalSharedDemandStep(ctx context.Context, r *protectionRecovery, p *terminalCleanupProgress) error {
	c := &r.checkpoint
	if c.Demand == nil {
		return storage.ErrCorruptObject
	}
	if c.Step == "demand_revision" {
		return s.records.WithBlobDemandLock(ctx, c.Demand.IntentID, func(ctx context.Context) error {
			intent, err := s.records.GetBlobIntent(ctx, c.Demand.IntentID)
			if errors.Is(err, storage.ErrNotFound) {
				c.Step = "demand_delete"
				return nil
			}
			if err == nil {
				p.Revision, c.DemandCursor, c.Step = intent.DemandRevision, inventory.Cursor{}, "demand_check"
			}
			return err
		})
	}
	if c.Step == "demand_check" {
		return s.checkTerminalSharedDemand(ctx, c)
	}
	return s.records.WithBlobDemandLock(ctx, c.Demand.IntentID, func(ctx context.Context) error {
		intent, err := s.records.GetBlobIntent(ctx, c.Demand.IntentID)
		if errors.Is(err, storage.ErrNotFound) {
			c.Step = "demand_delete"
			return nil
		}
		if err != nil {
			return err
		}
		if intent.DemandRevision != p.Revision {
			c.Step = "demand_revision"
			return nil
		}
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		if _, err := processor.cancelBlobIntentKnownUnowned(tasks.WithDeferredTerminalCleanup(ctx), intent.ID); err != nil {
			return err
		}
		c.Step = "demand_enqueue"
		return nil
	})
}
