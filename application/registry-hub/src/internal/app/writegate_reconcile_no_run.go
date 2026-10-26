package app

import (
	"context"
	"errors"
	"reflect"
	"registry-hub/internal/storage"
	"time"
)

func (g *writeGate) ReconcileFailedUnknownNoRun(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 || g.exclusive {
		return &gcReconciliationConflict{reason: "active Registry operations prevent GC gate reconciliation"}
	}
	if err := g.reloadLocked(ctx); err != nil {
		return err
	}
	g.signalLocked()
	if g.state.State == "open" {
		if g.state.ExecutorID == "" && !g.state.RecoveryBarrier {
			return nil
		}
		return &gcReconciliationConflict{reason: "the open GC gate contains unresolved execution state"}
	}
	if g.state.State != "failed_unknown" {
		return &gcReconciliationConflict{reason: "the GC gate is not in failed_unknown state"}
	}
	if g.state.ExecutorID != "" {
		return &gcReconciliationConflict{reason: "the gate records a GC executor whose state must be verified"}
	}
	if g.state.RecoveryBarrier {
		return &gcReconciliationConflict{reason: "solidification recovery is still required"}
	}
	if len(g.state.SessionIDs) != 0 || g.state.ReservedDistributionBytes != 0 {
		return &gcReconciliationConflict{reason: "active solidification protection prevents GC gate reconciliation"}
	}
	if err := g.validateProtectionStateLocked(ctx, true); err != nil {
		if errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrTimeout) ||
			errors.Is(err, storage.ErrPermissionDenied) || errors.Is(err, storage.ErrUnknownCommit) {
			return err
		}
		return &gcReconciliationConflict{reason: "solidification protection state is inconsistent: " + err.Error()}
	}

	g.state.State = "open"
	g.state.StartedAt = time.Time{}
	g.state.ErrorClass = ""
	g.state.RecoveryReason = ""
	g.state.ExecutorID = ""
	g.state.UpdatedAt = time.Now().UTC()
	expected := cloneGateRecord(g.state)
	if err := g.persistLocked(ctx); err == nil {
		g.signalLocked()
		return nil
	} else {
		return g.resolveReconciliationWrite(ctx, expected, err)
	}
}

func (g *writeGate) resolveReconciliationWrite(ctx context.Context, expected gateRecord, writeErr error) error {
	if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
		g.failClosedLocked("gc_reconciliation_unknown")
		return errors.Join(writeErr, reloadErr)
	}
	if reflect.DeepEqual(g.state, expected) {
		g.signalLocked()
		return nil
	}
	if errors.Is(writeErr, storage.ErrConditionFailed) {
		return &gcReconciliationConflict{reason: "the GC gate changed during reconciliation; inspect status and retry"}
	}
	if g.state.State != "failed_unknown" {
		g.failClosedLocked("gc_reconciliation_unknown")
	}
	return writeErr
}
