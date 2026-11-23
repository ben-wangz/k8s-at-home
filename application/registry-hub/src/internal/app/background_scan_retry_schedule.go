package app

import (
	"context"
	"errors"
	"reflect"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const (
	sourceRetryScheduleReconcileInterval = time.Minute
	sourceRetryScheduleProbeBudget       = 5 * time.Second
	sourceRetryScheduleAdmissionBudget   = 2 * time.Second
	sourceRetryScheduleBackoffMax        = time.Minute
	sourceRetryIndexRepairInterval       = time.Minute
	sourceRetryIndexRepairBudget         = 5 * time.Second
	sourceRetryIndexRepairLimit          = 16
)

type sourceRetryScheduleUpdate struct {
	dueAt time.Time
	stage string
	err   error
}

type sourceRetryDispatchState struct {
	last    time.Time
	after   time.Time
	backoff time.Duration
	turn    uint64
}

type sourceRetryIndexRepairState struct {
	nextAt  time.Time
	backoff time.Duration
}

func (s *Service) watchSourceRetrySchedule(ctx context.Context, kind string, updates chan sourceRetryScheduleUpdate) {
	wake := s.backgroundRetryWake[kind]
	timer := time.NewTimer(0)
	defer timer.Stop()
	backoff := time.Second
	probeFailed := false
	probeAt := time.Time{}
	for {
		woken := false
		select {
		case <-ctx.Done():
			return
		case <-wake:
			woken = true
		case <-timer.C:
		}
		now := time.Now()
		if probeFailed && now.Before(probeAt) || !woken && now.Before(probeAt) {
			resetSourceRetryTimer(timer, time.Until(probeAt))
			continue
		}
		dueAt, stage, err := s.probeSourceRetrySchedule(ctx, kind)
		if err != nil {
			publishSourceRetrySchedule(ctx, updates, sourceRetryScheduleUpdate{stage: stage, err: err})
			probeFailed = true
			probeAt = time.Now().Add(backoff)
			backoff = min(backoff*2, sourceRetryScheduleBackoffMax)
			resetSourceRetryTimer(timer, time.Until(probeAt))
			continue
		}
		publishSourceRetrySchedule(ctx, updates, sourceRetryScheduleUpdate{dueAt: dueAt})
		probeFailed = false
		backoff = time.Second
		probeAt = time.Now().Add(sourceRetryScheduleReconcileInterval)
		select {
		case <-wake:
			probeAt = time.Now()
		default:
		}
		resetSourceRetryTimer(timer, time.Until(probeAt))
	}
}

func (s *Service) probeSourceRetrySchedule(ctx context.Context, kind string) (time.Time, string, error) {
	admission, cancelAdmission := context.WithTimeout(ctx, sourceRetryScheduleAdmissionBudget)
	release, admitted := s.backgroundKindBudget(admission, kind)
	cancelAdmission()
	if !admitted {
		return time.Time{}, "retry_index_admission_yield", errBackgroundAdmissionYield
	}
	defer release()

	probe, cancelProbe := context.WithTimeout(ctx, sourceRetryScheduleProbeBudget)
	defer cancelProbe()
	probe = context.WithValue(probe, diagnosticContextKey{}, backgroundDiagnosticContext{
		Kind: kind, RunID: requestID(), Scope: "retry_index_discovery", Budget: sourceRetryScheduleProbeBudget.Seconds(),
	})
	dueAt, err := firstSourceRetryDueAt(probe, s.store, kind)
	return dueAt, "retry_index_read_failed", err
}

func (s *Service) runSourceRetryIndexRepair(ctx context.Context, holder lease.Lease, kind string, p *sourceSweepCheckpoint, backoff time.Duration) (time.Time, time.Duration) {
	now := time.Now()
	previousCursor := p.RetryIndexCursor
	nextBackoff := min(backoff*2, sourceRetryScheduleBackoffMax)
	if backoff <= 0 {
		backoff, nextBackoff = time.Second, 2*time.Second
	}
	deferNext := func(err error) (time.Time, time.Duration) {
		if ctx.Err() != nil {
			return time.Now(), backoff
		}
		event := "retry_index_repair_failed"
		if errors.Is(err, errBackgroundAdmissionYield) {
			event = "retry_index_admission_yield"
		}
		s.backgroundEvent(kind, event, err)
		return now.Add(backoff), nextBackoff
	}

	admission, cancelAdmission := context.WithTimeout(ctx, sourceRetryScheduleAdmissionBudget)
	release, admitted := s.backgroundKindBudget(admission, kind)
	cancelAdmission()
	if !admitted {
		return deferNext(errBackgroundAdmissionYield)
	}
	defer release()

	repairCtx, cancelRepair := context.WithTimeout(ctx, sourceRetryIndexRepairBudget)
	defer cancelRepair()
	repairCtx = context.WithValue(repairCtx, diagnosticContextKey{}, backgroundDiagnosticContext{
		Kind: kind, RunID: requestID(), Scope: "retry_index_repair", Budget: sourceRetryIndexRepairBudget.Seconds(),
	})
	repairCtx = inventory.WithWriteGuard(repairCtx, func(check context.Context) error {
		return lease.NewManager(s.store).Validate(check, holder)
	})
	if err := s.repairSourceRetryIndexes(repairCtx, kind, p, sourceRetryIndexRepairLimit); err != nil {
		return deferNext(err)
	}
	if reflect.DeepEqual(previousCursor, p.RetryIndexCursor) {
		return time.Now().Add(sourceRetryIndexRepairInterval), time.Second
	}

	checkpointCtx, cancelCheckpoint := s.backgroundCheckpoint(ctx, kind)
	p.Status.CheckpointAt = time.Now().UTC()
	err := s.saveSourceSweep(checkpointCtx, holder, kind, *p)
	cancelCheckpoint()
	if err != nil {
		return deferNext(err)
	}
	return time.Now().Add(sourceRetryIndexRepairInterval), time.Second
}

func (s *Service) runSourceRetryIndexRepairIfDue(ctx context.Context, holder lease.Lease, kind string, p *sourceSweepCheckpoint, state *sourceRetryIndexRepairState) {
	if !sourceSweepUsesDurableRetry(kind) || time.Now().Before(state.nextAt) {
		return
	}
	state.nextAt, state.backoff = s.runSourceRetryIndexRepair(ctx, holder, kind, p, state.backoff)
}

func firstSourceRetryDueAt(ctx context.Context, store storage.Store, kind string) (time.Time, error) {
	entries, _, err := store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 1)
	if err != nil || len(entries) == 0 {
		return time.Time{}, err
	}
	index, _, err := inventory.Read[sourceSweepRetryDue](ctx, store, entries[0].Key)
	if errors.Is(err, storage.ErrNotFound) {
		return time.Now(), nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return index.DueAt, nil
}

func publishSourceRetrySchedule(ctx context.Context, updates chan sourceRetryScheduleUpdate, update sourceRetryScheduleUpdate) {
	select {
	case updates <- update:
		return
	default:
	}
	select {
	case <-updates:
	default:
	}
	select {
	case updates <- update:
	case <-ctx.Done():
	}
}

func resetSourceRetryTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(max(0, delay))
}
