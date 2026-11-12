package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) runIntentSourceReconciliation(ctx context.Context) {
	s.runSourceSweep(ctx, "activation", "solidification/blobs/intents/", func(ctx context.Context, entry storage.Entry) error {
		intent, _, err := inventory.Read[tasks.BlobIntent](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		return s.reconcileBlobIntent(ctx, intent)
	})
}

func (s *Service) runTaskReconciliation(ctx context.Context) {
	graphs := map[string]inventory.Cursor{}
	s.runSourceSweep(ctx, "task_reconciliation", "tasks/state/", func(ctx context.Context, entry storage.Entry) error {
		task, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		gate := s.gate.State()
		if s.recoveryStarting.Load() || gate.RecoveryBarrier {
			return nil
		}
		if err := s.enqueueQuotaTransition(ctx, task); err != nil {
			return err
		}
		if err := s.wakeCoveredBlob(ctx, task); err != nil {
			return err
		}
		if isTerminalTask(task.Status) && !task.TerminalCleanupComplete {
			return processor.TaskTerminal(ctx, &task)
		}
		if gate.RecoveryBarrier || gate.State != "open" || task.CancelRequested {
			return nil
		}
		if task.Status == tasks.WaitingDeps && task.WaitReason == "distribution_verification" {
			return s.reconcileVerificationWait(ctx, task)
		}
		if task.Status == tasks.WaitingDeps && (task.WaitReason == "gc_admission" || task.WaitReason == "protection_transfer" || task.WaitReason == "manifest_scope_admission") {
			_, err = s.tasks.Wake(ctx, task.ID)
			return err
		}
		if task.Kind != tasks.RootKind || task.Status != tasks.WaitingDeps || task.WaitReason != "dependencies_unavailable" {
			delete(graphs, task.ID)
			return nil
		}
		cursor := graphs[task.ID]
		nodeEntry, ok, err := cursor.Peek(ctx, s.store, "solidification/graphs/"+task.ID+"/nodes/", 16)
		if err != nil {
			return err
		}
		if ok {
			node, _, err := inventory.Read[tasks.GraphNode](ctx, s.store, nodeEntry.Key)
			if err != nil {
				return err
			}
			if node.Role == tasks.GraphManifest {
				eligible, err := s.graphNodeNeedsAnchoredReuseReconciliation(ctx, task, node)
				if err != nil {
					cursor.Advance()
					graphs[task.ID] = cursor
					return err
				}
				if eligible {
					_, err = s.tasks.Wake(ctx, task.ID)
					delete(graphs, task.ID)
					return err
				}
			}
			cursor.Advance()
		}
		if cursor.Done {
			delete(graphs, task.ID)
		} else if _, tracked := graphs[task.ID]; tracked || len(graphs) < 1024 {
			graphs[task.ID] = cursor
		}
		return nil
	})
}

func (s *Service) runProtectionRecovery(ctx context.Context) {
	for ctx.Err() == nil {
		delay := 15 * time.Second
		if s.recoveryStarting.Load() || s.gate.State().RecoveryBarrier || s.gate.recovery != nil && s.gate.recovery.blocked.Load() {
			bounded, cancel := context.WithTimeout(ctx, time.Minute)
			bounded = context.WithValue(bounded, backgroundParentKey{}, ctx)
			err := s.recoverProtectionQuantum(bounded)
			if errors.Is(err, errProtectionRecoveryYield) {
				delay = 10 * time.Millisecond
			} else if errors.Is(err, lease.ErrHeld) || errors.Is(err, ErrProtectionBusy) {
				delay = time.Second
			} else if err != nil {
				delay = 2 * time.Second
				s.backgroundEvent("protection_recovery", "deferred", err)
				s.workerMu.Lock()
				if state := s.workerStates["protection_recovery"]; state.Recovery != nil {
					delay = max(delay, time.Until(state.Recovery.NextAttemptAt))
				}
				s.workerMu.Unlock()
			}
			cancel()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) runQuotaReconciliation(ctx context.Context) {
	s.runSourceSweepWithState(ctx, "quota_waiters", "tasks/state/", func(ctx context.Context, entry storage.Entry, checkpoint *sourceSweepCheckpoint) error {
		task, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		if checkpoint.QuotaCandidates == nil {
			checkpoint.QuotaCandidates = map[string]tasks.Task{}
		}
		selectQuotaCandidate(checkpoint.QuotaCandidates, task)
		return s.wakeCoveredBlob(ctx, task)
	}, s.completeQuotaScan)
}

func (s *Service) completeQuotaScan(ctx context.Context, checkpoint *sourceSweepCheckpoint) error {
	reasons := []string{"distribution_quota", "protected_cache_capacity"}
	for checkpoint.QuotaWakeIndex < len(reasons) {
		if !sourceBudgetAvailable(ctx) {
			return errSourceBudgetYield
		}
		reason := reasons[checkpoint.QuotaWakeIndex]
		if err := s.wakeQuotaCandidate(ctx, reason, checkpoint.QuotaCandidates[reason], s.gate.State()); err != nil {
			return err
		}
		checkpoint.QuotaWakeIndex++
	}
	checkpoint.QuotaLastCandidates = checkpoint.QuotaCandidates
	checkpoint.QuotaCandidates, checkpoint.QuotaWakeIndex = nil, 0
	return nil
}

func quotaCandidateBefore(left, right tasks.Task) bool {
	a, b := left.QuotaWaitAt, right.QuotaWaitAt
	if a.IsZero() {
		a = left.CreatedAt
	}
	if b.IsZero() {
		b = right.CreatedAt
	}
	if a.Equal(b) {
		return left.ID < right.ID
	}
	return a.Before(b)
}

func (s *Service) wakeQuotaCandidate(ctx context.Context, reason string, waiter tasks.Task, gate gateRecord) error {
	if waiter.ID == "" || gate.State != "open" || gate.RecoveryBarrier {
		return nil
	}
	current, err := s.tasks.Get(ctx, waiter.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil || current.Status != tasks.WaitingQuota || current.WaitReason != reason || current.CancelRequested || current.Generation != waiter.Generation || current.RoundID != waiter.RoundID {
		return err
	}
	return s.wakeFittingQuotaWaiter(ctx, reason, current, gate)
}
