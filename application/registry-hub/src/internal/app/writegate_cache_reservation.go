package app

import (
	"context"
	"errors"
	"time"
)

const maxProtectedCacheReservations = 10000

func (g *writeGate) HasProtectedCacheReservation(taskID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, exists := g.state.ProtectedCacheReservations[taskID]
	return exists
}

func (g *writeGate) ProtectedCacheReservation(taskID string) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reserved, exists := g.state.ProtectedCacheReservations[taskID]
	return reserved, exists
}

func (g *writeGate) ReserveProtectedCache(ctx context.Context, taskID string, bytes, limit int64) error {
	if !validGateID(taskID) || bytes < 0 || limit < 1 {
		return errors.New("protected cache reservation is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.State != "open" || g.exclusive {
		return ErrWritesClosed
	}
	if g.state.RecoveryBarrier {
		return ErrRecoveryBarrier
	}
	if g.state.ProtectedCacheReservations == nil {
		g.state.ProtectedCacheReservations = make(map[string]int64)
	}
	if current, exists := g.state.ProtectedCacheReservations[taskID]; exists {
		if current != bytes {
			return errors.New("protected cache reservation changed within a task round")
		}
		return nil
	}
	if g.state.ProtectedCacheBytes < 0 {
		return errors.New("protected cache coordinator total is negative")
	}
	if g.state.ProtectedCacheBytes > limit {
		return ErrProtectedCacheCapacity
	}
	if bytes > limit || bytes > limit-g.state.ProtectedCacheBytes {
		return ErrProtectedCacheCapacity
	}
	if len(g.state.ProtectedCacheReservations) >= maxProtectedCacheReservations {
		return ErrProtectionCapacity
	}
	previous := cloneGateRecord(g.state)
	g.state.ProtectedCacheReservations[taskID] = bytes
	g.state.ProtectedCacheBytes += bytes
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("protected_cache_reservation_unknown")
			return errors.Join(err, reloadErr)
		}
		if current, exists := g.state.ProtectedCacheReservations[taskID]; exists {
			if current == bytes {
				g.signalLocked()
				return nil
			}
			g.failClosedLocked("protected_cache_reservation_unknown")
			return err
		}
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) ReleaseProtectedCache(ctx context.Context, taskID string) error {
	if !validGateID(taskID) {
		return errors.New("protected cache reservation task ID is invalid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	bytes, exists := g.state.ProtectedCacheReservations[taskID]
	if !exists {
		return nil
	}
	if bytes < 0 || bytes > g.state.ProtectedCacheBytes {
		g.failClosedLocked("protected_cache_reservation_invalid")
		return errors.New("protected cache reservation exceeds coordinator total")
	}
	previous := cloneGateRecord(g.state)
	delete(g.state.ProtectedCacheReservations, taskID)
	g.state.ProtectedCacheBytes -= bytes
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("protected_cache_release_unknown")
			return errors.Join(err, reloadErr)
		}
		if _, stillReserved := g.state.ProtectedCacheReservations[taskID]; !stillReserved {
			g.signalLocked()
			return nil
		}
		return err
	}
	g.signalLocked()
	return nil
}

func (g *writeGate) ReconcileProtectedCacheReservations(ctx context.Context, active map[string]int64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.ProtectedCacheReservations == nil {
		g.state.ProtectedCacheReservations = make(map[string]int64)
	}
	previous := cloneGateRecord(g.state)
	changed := false
	for taskID, reserved := range g.state.ProtectedCacheReservations {
		if current, exists := active[taskID]; exists && current == reserved {
			continue
		}
		if reserved < 0 || reserved > g.state.ProtectedCacheBytes {
			g.failClosedLocked("protected_cache_reservation_invalid")
			return errors.New("protected cache reservation exceeds coordinator total")
		}
		delete(g.state.ProtectedCacheReservations, taskID)
		g.state.ProtectedCacheBytes -= reserved
		changed = true
	}
	if !changed {
		return nil
	}
	g.state.UpdatedAt = time.Now().UTC()
	if err := g.persistLocked(ctx); err != nil {
		if reloadErr := g.reloadLocked(ctx); reloadErr != nil {
			g.state = previous
			g.failClosedLocked("protected_cache_recovery_unknown")
			return errors.Join(err, reloadErr)
		}
		for taskID := range previous.ProtectedCacheReservations {
			if _, exists := g.state.ProtectedCacheReservations[taskID]; exists {
				return err
			}
		}
	}
	g.signalLocked()
	return nil
}
