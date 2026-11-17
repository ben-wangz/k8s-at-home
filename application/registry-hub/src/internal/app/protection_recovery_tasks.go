package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (r *protectionRecovery) taskStep(ctx context.Context) error {
	c := &r.checkpoint
	task, ok, err := r.recoveryTask(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if c.Cursor.Done {
			if c.Phase == "task_reconciliation" {
				r.next("task_reconciliation_blobs")
			} else {
				r.next("release_legacy")
			}
		}
		return nil
	}
	if c.Phase == "task_reconciliation" && task.Kind == tasks.BlobKind || c.Phase == "task_reconciliation_blobs" && task.Kind != tasks.BlobKind {
		r.finishTask()
		return nil
	}
	switch c.Step {
	case "":
		if task.Kind == tasks.BlobKind && task.ProtectedCacheBytes != 0 {
			if _, reserved := r.service.gate.recoverySnapshot().ProtectedCacheReservations[task.ID]; !reserved {
				if err := r.service.tasks.SetProtectedCacheBytes(ctx, task.ID, 0); err != nil {
					return err
				}
			}
		}
		c.SessionIDs = r.service.gate.recoverySnapshot().SessionIDs
		c.SessionIndex, c.Owned, c.ActiveIDs, c.ReservedBytes, c.ScopeID = 0, nil, nil, 0, task.ScopeID
		c.Step = "owners"
	case "owners":
		return r.taskOwnerStep(ctx, task)
	case "active_sessions":
		return r.activeTaskSessionStep(ctx, task)
	case "active_save":
		if err := r.saveActiveTask(ctx, task); err != nil {
			return r.service.failLegacyProtectionRecovery(ctx, task, err)
		}
		if isTerminalTask(task.Status) {
			c.Step = "terminal_demands"
		} else {
			r.finishTask()
		}
	case "terminal_demands", "demand_remove", "demand_check", "demand_cancel", "demand_delete":
		return r.terminalDemandStep(ctx, task)
	case "terminal_blob":
		return r.terminalBlobStep(ctx, task)
	case "terminal_sessions":
		return r.terminalSessionStep(ctx, task)
	case "terminal_finish":
		processor := &solidifier{service: r.service, tasks: r.service.tasks, gate: r.service.gate}
		processor.clearTaskProtection(&task)
		if err := r.service.tasks.SaveTerminalProtection(ctx, &task, false, ""); err != nil {
			return err
		}
		r.finishTask()
	default:
		return storage.ErrCorruptObject
	}
	return nil
}

func (r *protectionRecovery) taskOwnerStep(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if c.SessionIndex < len(c.SessionIDs) {
		id := c.SessionIDs[c.SessionIndex]
		session, active, err := r.service.gate.protectionSession(ctx, id)
		if err != nil {
			return err
		}
		if active {
			err := r.service.gate.verifyProtectionOwner(ctx, protectionOwnerKey(session.OwnerSetID, task.ID), session.OwnerSetID, task.ID)
			if err == nil {
				c.Owned = append(c.Owned, id)
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
		c.SessionIndex++
		return nil
	}
	c.SessionIDs = uniqueSessionIDs(append(append([]string(nil), task.SessionIDs...), c.Owned...))
	if task.Kind == tasks.BlobKind {
		intent, err := r.service.records.GetBlobIntent(ctx, task.ID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && intent.ProtectionSessionID != "" {
			c.SessionIDs = uniqueSessionIDs(append(c.SessionIDs, intent.ProtectionSessionID))
		}
	}
	if isLegacySolidificationTask(task) {
		c.SessionIDs = uniqueSessionIDs(append(c.SessionIDs, legacyGrandfatheredSessionID))
	}
	c.SessionIndex = 0
	if isTerminalTask(task.Status) && !(isLegacySolidificationTask(task) && !hasTaskProtectionReferences(task) && !safeLegacyTerminalTask(task)) {
		if hasLegacyRecoveryBlocker(task) && !hasTaskProtectionReferences(task) && !safeLegacyTerminalTask(task) {
			return ErrRecoveryBarrier
		}
		c.Step = "terminal_demands"
	} else {
		c.Step = "active_sessions"
	}
	return nil
}

func (r *protectionRecovery) activeTaskSessionStep(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if c.SessionIndex >= len(c.SessionIDs) {
		c.Step = "active_save"
		return nil
	}
	id := c.SessionIDs[c.SessionIndex]
	session, active, err := r.service.gate.protectionSession(ctx, id)
	if err != nil {
		return err
	}
	if active {
		if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil || session.RemoteWritePermit != "" || session.State != "protected" && session.State != "anchored" {
			return ErrRecoveryBarrier
		}
		if session.State == "protected" {
			if err := r.service.gate.AddProtectionOwnerDuringRecovery(ctx, id, task.ID); err != nil {
				return err
			}
		}
		if c.ScopeID == "" && !(isLegacySolidificationTask(task) && id == legacyGrandfatheredSessionID) {
			c.ScopeID = session.ScopeID
		}
		reserved := session.ReservedBytes
		if isLegacySolidificationTask(task) && id == legacyGrandfatheredSessionID {
			reserved = task.LegacyProtectionBytes
			if reserved == 0 {
				return errors.New("legacy task has no immutable grandfathered reservation")
			}
		}
		if reserved > math.MaxInt64-c.ReservedBytes {
			return storage.ErrCorruptObject
		}
		c.ReservedBytes += reserved
		c.ActiveIDs = append(c.ActiveIDs, id)
	}
	c.SessionIndex++
	return nil
}

func (r *protectionRecovery) saveActiveTask(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if len(c.ActiveIDs) == 0 {
		if isLegacySolidificationTask(task) {
			return errors.New("legacy task has no active grandfathered protection session")
		}
		return r.service.tasks.ReconcileProtectionState(ctx, task.ID, "", nil, 0, 0)
	}
	if c.ScopeID == "" && isLegacySolidificationTask(task) {
		var err error
		c.ScopeID, err = protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
		if err != nil {
			return err
		}
	}
	if c.ScopeID == "" {
		return errors.New("recovered task has no scope")
	}
	requested := c.ReservedBytes
	if isLegacySolidificationTask(task) && task.RequestedBytes > 0 {
		requested = task.RequestedBytes
	}
	return r.service.tasks.ReconcileProtectionState(ctx, task.ID, c.ScopeID, c.ActiveIDs, requested, c.ReservedBytes)
}

func (r *protectionRecovery) finishTask() {
	c := &r.checkpoint
	c.Cursor.Advance()
	c.Entity, c.Step, c.Inner, c.Demand, c.Half = "", "", inventory.Cursor{}, nil, 0
	c.SessionIndex, c.SessionIDs, c.Owned, c.ActiveIDs = 0, nil, nil, nil
}

func (r *protectionRecovery) terminalBlobStep(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if task.Kind != tasks.BlobKind {
		c.Step, c.Half = "terminal_sessions", 0
		return nil
	}
	switch c.Half {
	case 0:
		if task.Status == tasks.Cancelled {
			if err := r.service.records.RemoveBlobDemand(ctx, tasks.BlobDemand{IntentID: task.ID, DemandID: task.ID}); err != nil {
				return err
			}
		}
	case 1:
		state := tasks.IntentFailed
		if task.Status == tasks.Cancelled {
			state = tasks.IntentCancelled
		} else if task.Status == tasks.Solidified {
			state = tasks.IntentAnchored
		}
		if _, err := r.service.records.SetBlobIntentState(ctx, task.ID, state, task.ErrorClass, task.ErrorMessage); err != nil {
			return err
		}
	case 2:
		if err := r.service.gate.ReleaseProtectedCache(ctx, task.ID); err != nil {
			return err
		}
	case 3:
		if err := r.service.tasks.SetProtectedCacheBytes(ctx, task.ID, 0); err != nil {
			return err
		}
	default:
		c.Step, c.Half = "terminal_sessions", 0
		return nil
	}
	c.Half++
	return nil
}
