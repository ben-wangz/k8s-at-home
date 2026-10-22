package tasks

import (
	"context"
	"registry-hub/internal/lease"
	"time"
)

func (m *Manager) renewLease(ctx context.Context, cancel context.CancelFunc, current lease.Lease, done chan<- error) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			done <- nil
			return
		case <-ticker.C:
			renewed, err := m.leases.Renew(ctx, current, 60*time.Second)
			if err != nil {
				cancel()
				done <- err
				return
			}
			current = renewed
		}
	}
}
