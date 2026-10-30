package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"registry-hub/internal/lease"
)

var errBackgroundAdmissionYield = errors.New("background admission yielded")

const backgroundLeaseCallBudget = 5 * time.Second

func (s *Service) acquireBackgroundLease(parent context.Context, kind string, manager *lease.Manager, key, owner string, ttl time.Duration) (context.Context, lease.Lease, func() error, error) {
	ctx, finish, ok := s.beginBackgroundUnit(parent, kind, backgroundLeaseCallBudget)
	if !ok {
		return nil, lease.Lease{}, nil, errBackgroundAdmissionYield
	}
	current, err := manager.Acquire(ctx, key, owner, ttl)
	finish()
	if err != nil {
		return nil, lease.Lease{}, nil, err
	}
	return s.holdBackgroundLease(parent, kind, manager, current, ttl)
}

func (s *Service) holdBackgroundLease(parent context.Context, kind string, manager *lease.Manager, current lease.Lease, ttl time.Duration) (context.Context, lease.Lease, func() error, error) {
	return holdBackgroundLease(parent, kind, manager, current, ttl, func(stage string, err error) { s.backgroundEvent(kind, stage, err) })
}

func holdBackgroundLease(parent context.Context, kind string, manager *lease.Manager, current lease.Lease, ttl time.Duration, report func(string, error)) (context.Context, lease.Lease, func() error, error) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		err := renewBackgroundLease(ctx, cancel, kind, manager, current, ttl)
		if err != nil && report != nil {
			report("lease_renewal_failed", err)
		}
		done <- err
	}()
	var once sync.Once
	var result error
	releaseParent := parent
	if lifecycle, ok := parent.Value(backgroundParentKey{}).(context.Context); ok {
		releaseParent = lifecycle
	}
	release := func() error {
		once.Do(func() {
			cancel()
			renewErr := <-done
			// Shutdown leaves the lease to expire rather than starting detached storage work.
			var releaseErr error
			if releaseParent.Err() == nil {
				bounded, finish := backgroundLeaseContext(releaseParent, kind, "lease_release", backgroundLeaseCallBudget)
				releaseErr = manager.Release(bounded, current)
				finish()
			}
			result = errors.Join(renewErr, releaseErr)
			if releaseErr != nil && report != nil {
				report("lease_release_failed", releaseErr)
			}
		})
		return result
	}
	return ctx, current, release, nil
}

func (s *Service) acquireBackgroundUnitLease(parent context.Context, kind string, manager *lease.Manager, key, owner string, ttl time.Duration) (context.Context, lease.Lease, func() error, error) {
	return acquireAdmittedBackgroundLease(parent, kind, manager, key, owner, ttl, func(stage string, err error) { s.backgroundEvent(kind, stage, err) })
}

func acquireAdmittedBackgroundLease(parent context.Context, kind string, manager *lease.Manager, key, owner string, ttl time.Duration, report func(string, error)) (context.Context, lease.Lease, func() error, error) {
	ctx, finish := backgroundLeaseContext(parent, kind, "lease_acquisition", backgroundLeaseCallBudget)
	current, err := manager.Acquire(ctx, key, owner, ttl)
	finish()
	if err != nil {
		return nil, lease.Lease{}, nil, err
	}
	return holdBackgroundLease(parent, kind, manager, current, ttl, report)
}

func renewBackgroundLease(ctx context.Context, cancel context.CancelFunc, kind string, manager *lease.Manager, current lease.Lease, ttl time.Duration) error {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			bounded, finish := backgroundLeaseContext(ctx, kind, "lease_renewal", min(backgroundLeaseCallBudget, ttl/4))
			renewed, err := manager.Renew(bounded, current, ttl)
			finish()
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				cancel()
				return err
			}
			current = renewed
		}
	}
}

func backgroundLeaseContext(parent context.Context, kind, scope string, budget time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, budget)
	identity, _ := parent.Value(diagnosticContextKey{}).(backgroundDiagnosticContext)
	if kind != "" {
		identity.Kind = kind
	}
	identity.Scope, identity.Budget = scope, budget.Seconds()
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) < budget {
		identity.Scope = "external_deadline"
	}
	return context.WithValue(ctx, diagnosticContextKey{}, identity), cancel
}
