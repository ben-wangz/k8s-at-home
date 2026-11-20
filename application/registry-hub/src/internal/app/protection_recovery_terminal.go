package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

var errTerminalRemoteUnknown = errors.New("terminal task has an unresolved remote operation")

func (r *protectionRecovery) terminalDemandStep(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if task.Kind != tasks.RootKind {
		c.Step = "terminal_blob"
		return nil
	}
	s := r.service
	switch c.Step {
	case "terminal_demands":
		prefix := "solidification/blobs/root-owners/" + task.ID + "/"
		entry, ok, err := c.Inner.Peek(ctx, s.store, prefix, 16)
		if err != nil {
			return err
		}
		if !ok {
			if c.Inner.Done {
				c.Step = "terminal_blob"
			}
			return nil
		}
		demand, _, err := inventory.Read[tasks.BlobDemand](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			c.Inner.Advance()
			return nil
		}
		if err != nil {
			return err
		}
		if demand.SchemaVersion != 1 || demand.RootTaskID != task.ID || !validGateID(demand.IntentID) || !validGateID(demand.DemandID) || demand.ScopeID == "" || !validGateID(demand.SessionID) || demand.ReservedBytes < 0 {
			return storage.ErrCorruptObject
		}
		c.Demand, c.Step = &demand, "demand_remove"
	case "demand_remove":
		if err := s.records.RemoveBlobDemand(ctx, *c.Demand); err != nil {
			return err
		}
		c.DemandCursor, c.ActiveDemand, c.Step = inventory.Cursor{}, false, "demand_check"
	case "demand_check":
		return r.checkRemainingDemand(ctx)
	case "demand_cancel":
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		if _, err := processor.cancelBlobIntentKnownUnowned(tasks.WithDeferredTerminalCleanup(ctx), c.Demand.IntentID); err != nil {
			return err
		}
		c.Step = "demand_delete"
	case "demand_delete":
		if err := s.records.DeleteRootBlobDemandRecord(ctx, *c.Demand); err != nil {
			return err
		}
		c.Inner.Advance()
		c.Demand, c.Step = nil, "terminal_demands"
	}
	return nil
}

func (r *protectionRecovery) checkRemainingDemand(ctx context.Context) error {
	c := &r.checkpoint
	entry, ok, err := c.DemandCursor.Peek(ctx, r.service.store, "solidification/blobs/owners/"+c.Demand.IntentID+"/", 16)
	if err != nil {
		return err
	}
	if !ok {
		if c.DemandCursor.Done {
			c.Step = "demand_cancel"
		}
		return nil
	}
	demand, err := r.service.records.ReadRecoveryBlobDemand(ctx, entry.Key, c.Demand.IntentID)
	if err != nil {
		return err
	}
	active := demand.RootTaskID == ""
	if !active {
		root, err := r.service.tasks.Get(ctx, demand.RootTaskID)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		active = err == nil && !root.CancelRequested && !isTerminalTask(root.Status)
	}
	if active {
		c.ActiveDemand, c.Step = true, "demand_delete"
	} else {
		c.DemandCursor.Advance()
	}
	return nil
}

func (r *protectionRecovery) terminalSessionStep(ctx context.Context, task tasks.Task) error {
	c := &r.checkpoint
	if c.SessionIndex >= len(c.SessionIDs) {
		if err := r.clearInactiveTerminalBlobProtection(ctx, task); err != nil {
			return err
		}
		c.Step = "terminal_finish"
		return nil
	}
	id := c.SessionIDs[c.SessionIndex]
	g := r.service.gate
	session, active, err := g.protectionSession(ctx, id)
	if err != nil {
		return err
	}
	if !active {
		if err := g.confirmInactiveProtectionSessionReleased(ctx, id, "", ""); err != nil {
			return err
		}
		c.SessionIndex++
		c.Half = 0
		return nil
	}
	if session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil || session.RemoteWritePermit != "" {
		if _, terminal := ctx.Value(terminalWriteScopeKey{}).(*terminalWriteScope); terminal && (session.UncertainRemoteResult || session.RemoteWrite != nil) {
			return errTerminalRemoteUnknown
		}
		return ErrProtectionBusy
	}
	if id == legacyGrandfatheredSessionID && (session.ReservationClass != "legacy" || session.ScopeID != legacyGrandfatheredScopeID) {
		return storage.ErrCorruptObject
	}
	if task.Status == tasks.Solidified && id != legacyGrandfatheredSessionID {
		if session.State != "anchored" {
			return g.MarkProtectionAnchored(ctx, id)
		}
		if err := g.ReleaseProtection(ctx, id, "anchored"); err != nil {
			return err
		}
		c.SessionIndex++
		c.Half = 0
		return nil
	}
	if c.Half == 0 {
		remaining, err := g.RemoveProtectionOwner(ctx, id, task.ID)
		if err != nil {
			return err
		}
		if remaining > 0 {
			c.SessionIndex++
		} else {
			c.Half = 1
		}
		return nil
	}

	switch session.State {
	case "anchored":
		err = g.ReleaseProtection(ctx, id, "anchored")
	case "quiesced":
		err = g.ReleaseProtection(ctx, id, terminalProtectionReason(task.Status))
	case "quiescing":
		return g.MarkProtectionQuiesced(ctx, id)
	case "stop_requested", "recovery_required":
		return g.MarkProtectionQuiescing(ctx, id)
	default:
		stopped, err := g.RequestProtectionStop(ctx, id, terminalProtectionReason(task.Status))
		if err == nil && !stopped {
			return ErrProtectionBusy
		}
		return err
	}
	if err != nil {
		return err
	}
	c.SessionIndex++
	c.Half = 0
	return nil
}

func (r *protectionRecovery) clearInactiveTerminalBlobProtection(ctx context.Context, task tasks.Task) error {
	if task.Kind != tasks.BlobKind {
		return nil
	}
	intent, err := r.service.records.GetBlobIntent(ctx, task.ID)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.ProtectionSessionID == "" {
		return nil
	}
	_, active, err := r.service.gate.protectionSession(ctx, intent.ProtectionSessionID)
	if err != nil || active {
		return err
	}
	return r.service.records.ClearBlobIntentProtectionSession(ctx, intent.ID, intent.ProtectionSessionID)
}
