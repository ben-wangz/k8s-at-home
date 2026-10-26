package app

import (
	"context"
	"errors"
	"time"
)

func (g *writeGate) SetGCState(ctx context.Context, state, executorID, errorClass string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.state.State = state
	g.state.ExecutorID = executorID
	g.state.ErrorClass = errorClass
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.state.ErrorClass = "storage"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) Open(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active != 0 || g.state.State != "restore" {
		return errors.New("GC gate cannot be opened from its current state")
	}
	if g.state.RecoveryBarrier || len(g.state.SessionIDs) != 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	g.state.State = "open"
	g.state.StartedAt = time.Time{}
	g.state.ErrorClass = ""
	g.state.RecoveryReason = ""
	g.state.ExecutorID = ""
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.state.ErrorClass = "storage"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) persist(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.persistLocked(ctx)
}
