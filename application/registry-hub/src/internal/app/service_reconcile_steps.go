package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
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
		if isTerminalTask(task.Status) && !task.TerminalCleanupComplete {
			return processor.TaskTerminal(ctx, &task)
		}
		gate := s.gate.State()
		if gate.RecoveryBarrier || gate.State != "open" || task.CancelRequested {
			return nil
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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for ctx.Err() == nil {
		if s.gate.State().RecoveryBarrier {
			bounded, cancel := context.WithTimeout(ctx, time.Minute)
			if err := s.recoverSolidificationProtection(bounded); err != nil {
				s.backgroundEvent("protection_recovery", "deferred", err)
			}
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) runQuotaReconciliation(ctx context.Context) {
	oldest := map[string]tasks.Task{}
	lastKey := ""
	s.runSourceSweep(ctx, "quota_waiters", "tasks/state/", func(ctx context.Context, entry storage.Entry) error {
		if entry.Key <= lastKey {
			for reason, candidate := range oldest {
				if err := s.wakeQuotaCandidate(ctx, reason, candidate, s.gate.State()); err != nil {
					return err
				}
			}
			oldest = map[string]tasks.Task{}
		}
		lastKey = entry.Key
		task, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
		if err != nil {
			return err
		}
		reason := task.WaitReason
		eligible := task.Status == tasks.WaitingQuota
		if (task.Status == tasks.Pending || task.Status == tasks.Running) && !task.QuotaWaitAt.IsZero() {
			reason, eligible = task.QuotaWaitReason, true
		}
		if eligible && !task.CancelRequested && (reason == "distribution_quota" || reason == "protected_cache_capacity") {
			current := oldest[reason]
			if current.ID == "" || quotaCandidateBefore(task, current) {
				oldest[reason] = task
			}
		}
		return nil
	})
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
	if gate.State != "open" || gate.RecoveryBarrier {
		return nil
	}
	current, err := s.tasks.Get(ctx, waiter.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil || current.Status != tasks.WaitingQuota || current.WaitReason != reason || current.CancelRequested {
		return err
	}
	return s.wakeFittingQuotaWaiter(ctx, reason, current, gate)
}
