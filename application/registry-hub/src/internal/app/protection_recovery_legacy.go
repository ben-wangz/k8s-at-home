package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/inventory"
	"sort"
)

func (r *protectionRecovery) legacyStep(ctx context.Context) error {
	c := &r.checkpoint
	s := r.service
	session, active, err := s.gate.protectionSession(ctx, legacyGrandfatheredSessionID)
	if err != nil {
		return err
	}
	if active && (session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID) {
		return errors.New("legacy grandfathered session has an incompatible identity")
	}
	switch c.Phase {
	case "legacy_owners":
		if active {
			prefix := "system/solidification/owners/" + session.OwnerSetID + "/"
			entry, ok, err := c.Cursor.Peek(ctx, s.store, prefix, 16)
			if err != nil {
				return err
			}
			if ok {
				id, err := recoveryEntryID(entry, prefix)
				if err != nil {
					return err
				}
				if err := s.gate.verifyProtectionOwner(ctx, entry.Key, session.OwnerSetID, id); err != nil {
					return err
				}
				if len(c.LegacyOwners) >= 10000 {
					return ErrProtectionCapacity
				}
				c.LegacyOwners[id] = true
				c.Cursor.Advance()
				return nil
			}
			if !c.Cursor.Done {
				return nil
			}
		}
		r.next("legacy_tasks")
	case "legacy_tasks":
		return r.legacyTaskStep(ctx)
	case "legacy_admit":
		if active && session.ReservedBytes < c.LegacyBytes {
			return errors.New("legacy reservation is smaller than its active task scopes")
		}
		if active && session.State == "anchored" {
			if c.LegacyBytes > 0 {
				return errors.New("active legacy tasks reference an anchored grandfathered session")
			}
			r.next("task_reconciliation")
			return nil
		}
		if c.LegacyBytes == 0 && !active {
			r.next("task_reconciliation")
			return nil
		}
		reserved := c.LegacyBytes
		if active {
			reserved = session.ReservedBytes
		}
		maxSessions, _ := protectionLimits(s.cfg)
		if err := s.gate.AdmitProtectionDuringRecovery(ctx, legacyGrandfatheredSessionID, legacyGrandfatheredScopeID, "", reserved, maxSessions, r.legacyLimit()); err != nil {
			return err
		}
		r.next("legacy_link")
	case "legacy_link":
		owners := make([]string, 0, len(c.LegacyRequired))
		for id := range c.LegacyRequired {
			owners = append(owners, id)
		}
		sort.Strings(owners)
		if c.Index < len(owners) {
			if err := s.gate.AddProtectionOwnerDuringRecovery(ctx, legacyGrandfatheredSessionID, owners[c.Index]); err != nil {
				return err
			}
			c.Index++
			return nil
		}
		r.next("task_reconciliation")
	}
	return nil
}

func (r *protectionRecovery) legacyLimit() int64 {
	if limit := r.service.cfg.Limits.ClosureBytes; limit > 0 {
		return limit
	}
	return 1 << 40
}

func (r *protectionRecovery) legacyTaskStep(ctx context.Context) error {
	c := &r.checkpoint
	s := r.service
	task, ok, err := r.recoveryTask(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if c.Cursor.Done {
			if c.LegacySeen != len(c.LegacyOwners) {
				return errors.New("legacy session owner has no matching task")
			}
			r.next("legacy_admit")
		}
		return nil
	}
	hasOwner := c.LegacyOwners[task.ID]
	if !isLegacySolidificationTask(task) {
		if hasOwner {
			return errors.New("legacy session owner is not a legacy root task")
		}
	} else {
		if hasOwner {
			c.LegacySeen++
		}
		if legacyTaskNeedsProtection(task, hasOwner) && !(hasOwner && isTerminalTask(task.Status) && safeLegacyTerminalTask(task)) {
			if len(task.Checkpoints) == 0 {
				return s.failLegacyProtectionRecovery(ctx, task, errors.New("legacy root may contain an uncheckpointed Distribution write"))
			}
			reserved := task.LegacyProtectionBytes
			if reserved == 0 {
				reserved, err = taskGraphBytes(task)
				if err != nil {
					return s.failLegacyProtectionRecovery(ctx, task, err)
				}
				if err := s.tasks.SetLegacyProtectionBytes(ctx, task.ID, reserved); err != nil {
					return err
				}
			}
			if reserved < 1 || reserved > r.legacyLimit() || reserved > math.MaxInt64-c.LegacyBytes || len(c.LegacyRequired) >= 10000 {
				return s.failLegacyProtectionRecovery(ctx, task, ErrProtectionCapacity)
			}
			c.LegacyRequired[task.ID] = true
			c.LegacyBytes += reserved
		}
	}
	c.Cursor.Advance()
	c.Entity, c.Inner = "", inventory.Cursor{}
	return nil
}
