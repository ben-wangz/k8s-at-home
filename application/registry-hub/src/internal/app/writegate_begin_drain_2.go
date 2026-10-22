package app

import (
	"context"
	"errors"
	"sync"
	"time"
)

func (g *writeGate) BeginDrain(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if err := g.validateProtectionStateLocked(ctx, false); err != nil {
		return err
	}
	if len(g.state.SessionIDs) > 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	if g.state.Epoch == ^uint64(0) {
		return errors.New("GC epoch is exhausted")
	}
	g.state.Epoch++
	g.state.State = "draining"
	g.state.StartedAt = time.Now().UTC()
	g.state.UpdatedAt = g.state.StartedAt
	if err := g.persistLocked(ctx); err != nil {
		g.state.State = "failed_unknown"
		g.signalLocked()
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) BeginRetry(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.exclusive || g.state.State != "failed_unknown" && g.state.State != "failed" {
		return errors.New("GC retry requires a failed or unknown state")
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if err := g.validateProtectionStateLocked(ctx, false); err != nil {
		return err
	}
	if len(g.state.SessionIDs) > 0 || g.state.ReservedDistributionBytes != 0 {
		return ErrGCProtected
	}
	if g.state.Epoch == ^uint64(0) {
		return errors.New("GC epoch is exhausted")
	}
	g.state.Epoch++
	g.state.State = "draining"
	g.state.ErrorClass = ""
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

func (g *writeGate) BeginExclusive(ctx context.Context, timeout time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	g.mu.Lock()
	if g.state.State != "open" || g.exclusive {
		g.mu.Unlock()
		return nil, ErrWritesClosed
	}
	g.exclusive = true
	g.signalLocked()
	for g.active > 0 {
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.clearExclusive()
			return nil, ctx.Err()
		case <-deadline.C:
			g.clearExclusive()
			return nil, errors.New("timed out draining writes for exclusive operation")
		case <-changed:
		}
		g.mu.Lock()
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(g.clearExclusive)
	}, nil
}

func (g *writeGate) clearExclusive() {
	g.mu.Lock()
	g.exclusive = false
	g.signalLocked()
	g.mu.Unlock()
}

func (g *writeGate) WaitQuiesced(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		g.mu.Lock()
		if g.state.State != "draining" {
			state := g.state.State
			g.mu.Unlock()
			return errors.New("GC gate left draining state: " + state)
		}
		if g.active == 0 && len(g.state.SessionIDs) == 0 && g.state.ReservedDistributionBytes == 0 {
			g.state.State = "backend_quiesced"
			g.state.UpdatedAt = time.Now().UTC()
			err := g.persistLocked(ctx)
			if err != nil {
				g.state.State = "failed_unknown"
				g.state.ErrorClass = "storage"
			}
			g.signalLocked()
			g.mu.Unlock()
			return err
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out draining active Registry writes")
		case <-changed:
		}
	}
}

func (g *writeGate) WaitRecoveryQuiesced(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		g.mu.Lock()
		if !g.state.RecoveryBarrier {
			g.mu.Unlock()
			return errors.New("recovery barrier is not active")
		}
		if g.active == 0 {
			g.mu.Unlock()
			return nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("timed out waiting for active Registry operations before recovery")
		case <-changed:
		}
	}
}
