package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"sync"
	"time"
)

func acquireProtectionRecoveryLease(parent context.Context, manager *lease.Manager, owner string) (context.Context, lease.Lease, func() error, error) {
	bounded, cancel := context.WithTimeout(parent, protectionRecoveryCheckpointBudget)
	held, err := manager.Acquire(bounded, "protection-recovery", owner, 90*time.Second)
	cancel()
	if err != nil {
		return nil, lease.Lease{}, nil, err
	}
	ctx, stop := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		current := held
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				bounded, cancel := context.WithTimeout(ctx, protectionRecoveryCheckpointBudget)
				renewed, err := manager.Renew(bounded, current, 90*time.Second)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						done <- nil
						return
					}
					stop()
					done <- err
					return
				}
				current = renewed
			}
		}
	}()
	var once sync.Once
	var result error
	release := func() error {
		once.Do(func() {
			stop()
			renewErr := <-done
			lifecycle := parent
			if outer, ok := parent.Value(backgroundParentKey{}).(context.Context); ok {
				lifecycle = outer
			}
			if lifecycle.Err() != nil {
				result = renewErr
				return
			}
			bounded, cancel := context.WithTimeout(lifecycle, protectionRecoveryCheckpointBudget)
			defer cancel()
			result = errors.Join(renewErr, manager.Release(bounded, held))
		})
		return result
	}
	return ctx, held, release, nil
}
