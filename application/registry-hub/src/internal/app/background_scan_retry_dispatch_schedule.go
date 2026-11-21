package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"time"
)

func (s *Service) dispatchScheduledSourceRetry(ctx context.Context, holder lease.Lease, kind string, p *sourceSweepCheckpoint, step sourceSweepStep, dueAt time.Time, scanDue bool, state *sourceRetryDispatchState, lastStatus *time.Time) (bool, error) {
	state.turn++
	now := time.Now()
	if dueAt.IsZero() || dueAt.After(now) || now.Before(state.after) || scanDue && state.turn%4 != 0 && time.Since(state.last) < time.Second {
		return false, nil
	}
	if state.backoff <= 0 {
		state.backoff = time.Second
	}

	bounded, release, admitted := s.beginSourceSweepUnit(ctx, kind)
	state.last = time.Now()
	if !admitted {
		state.after = state.last.Add(state.backoff)
		state.backoff = min(state.backoff*2, 30*time.Second)
		return false, nil
	}
	defer release()
	guarded := inventory.WithWriteGuard(bounded, func(check context.Context) error {
		return lease.NewManager(s.store).Validate(check, holder)
	})
	processed, err := s.dispatchDueSourceSweepRetry(guarded, holder, kind, p, step)
	if err != nil {
		s.backgroundEvent(kind, "retry_dispatch_failed", err)
	}
	if err != nil || !processed {
		state.after = state.last.Add(state.backoff)
		state.backoff = min(state.backoff*2, 30*time.Second)
	} else {
		state.after = state.last.Add(time.Second)
		state.backoff = time.Second
	}
	if processed {
		statusCtx, cancel := s.backgroundCheckpoint(ctx, kind)
		var checkpointErr error
		if lastStatus.IsZero() || time.Since(*lastStatus) >= 5*time.Second {
			if err := s.updateSourceRetryStatus(statusCtx, kind, p); err != nil {
				checkpointErr = err
			} else {
				*lastStatus = time.Now()
			}
		}
		if checkpointErr == nil {
			markSourceRetryRoundSuccess(p)
			p.Status.CheckpointAt = time.Now().UTC()
			checkpointErr = s.saveSourceSweep(statusCtx, holder, kind, *p)
		}
		cancel()
		if checkpointErr != nil {
			s.backgroundEvent(kind, "checkpoint_failed", checkpointErr)
			return false, checkpointErr
		}
		return true, nil
	}
	s.signalSourceRetry(kind)
	return false, nil
}
