package tasks

import (
	"context"
	"time"
)

type terminalChangeGuard interface {
	WithTerminalChange(context.Context, string, func(context.Context) error) error
}

func (m *Manager) Cancel(ctx context.Context, id string) (task Task, result error) {
	if guard, ok := m.processor.(terminalChangeGuard); ok {
		result = guard.WithTerminalChange(ctx, id, func(ctx context.Context) error {
			task, result = m.cancelTask(ctx, id)
			return result
		})
		return task, result
	}
	return m.cancelTask(ctx, id)
}

func (m *Manager) CancelIfExpired(ctx context.Context, id string, cutoff time.Time) (cancelled bool, result error) {
	if guard, ok := m.processor.(terminalChangeGuard); ok {
		result = guard.WithTerminalChange(ctx, id, func(ctx context.Context) error {
			cancelled, result = m.cancelExpiredTask(ctx, id, cutoff)
			return result
		})
		return cancelled, result
	}
	return m.cancelExpiredTask(ctx, id, cutoff)
}
