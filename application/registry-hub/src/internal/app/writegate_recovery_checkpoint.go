package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func recoveryLedger(state gateRecord) string {
	state.UpdatedAt = time.Time{}
	state.RecoveryBarrier, state.RecoveryReason = false, ""
	state.RecoveryID, state.RecoveryFence = "", 0
	state.ErrorClass = ""
	state.RecoveryTrigger = nil
	data, _ := json.Marshal(state)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (g *writeGate) recoverySnapshot() gateRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return cloneGateRecord(g.state)
}

func (g *writeGate) claimProtectionRecovery(ctx context.Context, fence uint64) (gateRecord, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	previous := cloneGateRecord(g.state)
	if err := g.reloadLocked(ctx); err != nil {
		return gateRecord{}, err
	}
	// A local failure may reflect a failed barrier write, while durable GC evidence is still open.
	if previous.State == "failed_unknown" && g.state.State == "open" {
		previous.State = "open"
		if recoveryLedger(previous) != recoveryLedger(g.state) {
			g.state = previous
			g.failClosedLocked("recovery_ledger_conflict")
			return gateRecord{}, errors.New("local and durable protection ledgers require reconciliation")
		}
	}
	if g.active != 0 || g.exclusive {
		return gateRecord{}, ErrProtectionBusy
	}
	if !g.state.RecoveryBarrier || g.state.RecoveryID == "" {
		g.state.RecoveryID = requestID()
	}
	if previous.RecoveryBarrier && previous.RecoveryTrigger != nil && (!g.state.RecoveryBarrier || g.state.RecoveryTrigger == nil) {
		g.state.RecoveryTrigger = previous.RecoveryTrigger
		if previous.RecoveryID != "" {
			g.state.RecoveryID = previous.RecoveryID
		}
	}
	if !previous.RecoveryBarrier && !g.state.RecoveryBarrier {
		g.state.RecoveryTrigger = barrierTrigger(ctx, "startup_reconcile")
	} else if g.state.RecoveryTrigger == nil {
		g.state.RecoveryTrigger = &recoveryTrigger{Kind: "unknown", Reason: "trigger_evidence_missing"}
	}
	g.state.RecoveryFence = fence
	g.state.RecoveryBarrier, g.state.RecoveryReason = true, "startup_reconcile"
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("recovery_claim_unknown")
		return gateRecord{}, err
	}
	g.signalLocked()
	return cloneGateRecord(g.state), nil
}

func (g *writeGate) completeProtectionRecovery(ctx context.Context, generation string, fence uint64, ledger string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if guard, ok := ctx.Value(recoveryWriteGuardKey{}).(recoveryWriteGuard); ok {
		if err := guard(ctx); err != nil {
			return err
		}
	}
	if err := g.reloadLocked(ctx); err != nil {
		return err
	}
	if !g.state.RecoveryBarrier || g.state.RecoveryID != generation || g.state.RecoveryFence != fence || g.active != 0 || g.exclusive || recoveryLedger(g.state) != ledger {
		return storage.ErrConditionFailed
	}
	g.state.RecoveryBarrier, g.state.RecoveryReason = false, ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.failClosedLocked("recovery_completion_unknown")
		return err
	}
	g.signalLocked()
	return nil
}

func readRecoveryGate(ctx context.Context, store storage.Store) (gateRecord, error) {
	state, _, err := inventory.Read[gateRecord](ctx, store, "system/gc/state.json")
	if err == nil && (state.SchemaVersion != gateSchemaCurrent || state.Epoch == 0) {
		err = storage.ErrCorruptObject
	}
	return state, err
}
