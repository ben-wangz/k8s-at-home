package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) terminalQuotaStep(ctx context.Context, c *protectionRecoveryCheckpoint, p *terminalCleanupProgress) error {
	if p.QuotaWaiters == nil {
		p.QuotaWaiters = map[string]string{}
	}
	if c.Step == "terminal_quota_wake" {
		reasons := []string{"distribution_quota", "protected_cache_capacity"}
		if p.QuotaIndex >= len(reasons) {
			c.Step = "terminal_finish"
			return nil
		}
		reason := reasons[p.QuotaIndex]
		if id := p.QuotaWaiters[reason]; id != "" {
			candidate, err := s.tasks.Get(ctx, id)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if err == nil {
				if err := s.wakeQuotaCandidate(ctx, reason, candidate, s.gate.State()); err != nil {
					return err
				}
			}
		}
		p.QuotaIndex++
		return nil
	}
	entry, ok, err := c.Cursor.Peek(ctx, s.store, "tasks/state/", 16)
	if err != nil {
		return err
	}
	if !ok {
		if c.Cursor.Done {
			c.Step = "terminal_quota_wake"
		}
		return nil
	}
	task, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		c.Cursor.Advance()
		return nil
	}
	if err != nil {
		return err
	}
	reason := task.WaitReason
	if task.Status == tasks.WaitingQuota && !task.CancelRequested && (reason == "distribution_quota" || reason == "protected_cache_capacity") {
		prior, err := s.tasks.Get(ctx, p.QuotaWaiters[reason])
		if p.QuotaWaiters[reason] == "" || errors.Is(err, storage.ErrNotFound) {
			p.QuotaWaiters[reason] = task.ID
		} else if err != nil {
			return err
		} else if quotaCandidateBefore(task, prior) {
			p.QuotaWaiters[reason] = task.ID
		}
	}
	c.Cursor.Advance()
	return nil
}
