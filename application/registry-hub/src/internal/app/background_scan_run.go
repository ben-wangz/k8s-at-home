package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) runSourceSweepStateLeader(ctx context.Context, holder lease.Lease, kind, prefix string, step sourceSweepStep, end sourceSweepEnd) {
	initial, cancel := context.WithTimeout(ctx, 5*time.Second)
	p, _, err := inventory.Read[sourceSweepCheckpoint](initial, s.store, sourceSweepKey(kind))
	cancel()
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		s.backgroundEvent(kind, "checkpoint_read_failed", err)
		return
	}
	if p.Backoff == nil {
		p.Backoff = map[string]entityBackoff{}
	}
	if p.Round == 0 {
		p.Round = 1
	}
	if p.Status.StartedAt.IsZero() {
		p.Status.StartedAt = time.Now().UTC()
	}
	p.Status.Owner, p.Status.Fence, p.Status.Resumability = holder.Owner, holder.FencingToken, "durable_checkpoint"
	if p.Status.RunID == "" {
		p.Status.RunID = requestID()
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var signalCheck time.Time
	var lastSave time.Time
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-s.backgroundWake[kind]:
			p.IdleRounds = 0
			if p.NextRound.After(time.Now()) {
				p.NextRound = time.Now().Add(backgroundScanWakeDelay)
			}
		case <-ticker.C:
		}
		if kind == "quota_waiters" && time.Since(signalCheck) >= 5*time.Second {
			checkCtx, checkCancel := context.WithTimeout(ctx, 2*time.Second)
			err = s.refreshQuotaSignal(checkCtx, &p)
			checkCancel()
			signalCheck = time.Now()
			if err != nil {
				s.backgroundEvent(kind, "signal_read_failed", err)
			}
		}
		if p.Status.WaitReason == "protection_recovery" && !s.recoveryBlocked() {
			p.NextRound = time.Time{}
		}
		if time.Now().Before(p.NextRound) {
			continue
		}
		bounded, release, ok := s.beginSourceSweepUnit(ctx, kind)
		if !ok {
			continue
		}
		guarded := inventory.WithWriteGuard(bounded, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
		p.Status.LastHeartbeatAt = time.Now().UTC()
		if recoveryWriter(kind) && s.recoveryBlocked() {
			s.sourceDependencyWait(guarded, kind, &p, errRecoveryPaused)
		} else {
			if p.Status.WaitReason == "protection_recovery" {
				p.Status.WaitReason, p.Status.DependencyGeneration = "", ""
			}
			s.sourceSweepBatch(guarded, kind, prefix, &p, step)
		}
		if p.Cursor.Done && p.Status.WaitReason == "" && sourceBudgetAvailable(guarded) {
			if end != nil {
				err = end(guarded, &p)
			} else {
				err = nil
			}
			if err == nil {
				s.completeSourceRound(kind, &p)
			} else {
				s.failSourceScan(guarded, kind, prefix, &p, err)
			}
		}
		p.Status.Processed, p.Status.NextScanAt = p.Cursor.Entities, p.NextRound
		var checkpointErr error
		if kind == "quota_waiters" || p.Cursor.Entities == 0 || p.Status.WaitReason != "" || time.Since(lastSave) >= 5*time.Second {
			checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, kind)
			p.Status.CheckpointAt = time.Now().UTC()
			checkpointErr = s.saveSourceSweep(checkpointCtx, holder, kind, p)
			checkpointCancel()
			lastSave = time.Now()
		}
		release()
		s.updateWorker(kind, func(state *workerStatus) {
			state.Resumability, state.LastEntityProgressAt, state.ErrorClass = "durable_checkpoint", p.Status.LastEntityProgressAt, p.Status.ErrorClass
			state.WaitReason, state.DependencyGeneration, state.DependencyCheckedAt = p.Status.WaitReason, p.Status.DependencyGeneration, p.Status.DependencyCheckedAt
			if p.Status.WaitReason != "" {
				state.State = "waiting_dependency"
			}
		})
		if checkpointErr != nil {
			s.backgroundEvent(kind, "checkpoint_failed", checkpointErr)
			return
		}
	}
}

func (s *Service) sourceSweepBatch(ctx context.Context, kind, prefix string, p *sourceSweepCheckpoint, step sourceSweepStep) {
	limit := 1
	if kind == "quota_waiters" {
		limit = 16
	}
	for unit := 0; unit < limit && sourceBudgetAvailable(ctx); unit++ {
		if !s.sourceSweepEntity(ctx, kind, prefix, p, step) {
			break
		}
	}
}

func (s *Service) completeSourceRound(kind string, p *sourceSweepCheckpoint) {
	for id, b := range p.Backoff {
		if b.Round != p.Round {
			delete(p.Backoff, id)
		}
	}
	status := &p.Status
	status.Complete, status.ObservedAt = len(p.Backoff) == 0 && !p.RoundFailed, time.Now().UTC()
	status.LastScanCompletedAt = status.ObservedAt
	s.metrics.recordBackgroundScanRound(kind, status.Complete)
	if status.Complete {
		recordScanRecovery(kind, "source_scan", status, &p.DiagnosticOutbox, "")
		status.LastSuccessAt, status.ErrorRecovered, status.NextAttemptAt = status.ObservedAt, !status.ErrorAt.IsZero(), time.Time{}
	}
	p.Round++
	retry := p.RoundFailed || len(p.Backoff) > 0
	p.RoundFailed, p.Cursor = false, inventory.Cursor{}
	if retry {
		p.IdleRounds = 0
	} else {
		p.IdleRounds++
	}
	p.NextRound = time.Now().Add(sourceSweepNextDelay(kind, p.IdleRounds, retry))
}
