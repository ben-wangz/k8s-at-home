package tasks

import (
	"context"
	"time"
)

type deferredTerminalCleanupKey struct{}

func WithDeferredTerminalCleanup(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredTerminalCleanupKey{}, true)
}

func (m *Manager) PauseForRecovery(ctx context.Context) error {
	m.recoveryPaused.Store(true)
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		for _, cancel := range m.cancels {
			cancel()
		}
		active := len(m.active)
		m.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) ResumeAfterRecovery() { m.recoveryPaused.Store(false) }
