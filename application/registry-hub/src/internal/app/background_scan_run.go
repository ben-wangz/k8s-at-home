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
	initial = inventory.WithWriteGuard(initial, func(check context.Context) error {
		return lease.NewManager(s.store).Validate(check, holder)
	})
	p, _, err := inventory.Read[sourceSweepCheckpoint](initial, s.store, sourceSweepKey(kind))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		cancel()
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
	if err := importSourceSweepBackoff(initial, s, kind, &p, 16); err != nil {
		cancel()
		s.backgroundEvent(kind, "retry_migration_failed", err)
		return
	}
	cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var signalCheck time.Time
	var lastSave time.Time
	var lastRetryDispatch time.Time
	var lastRetryStatus time.Time
	var retryTurn uint64
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-s.backgroundWake[kind]:
			p.IdleRounds = 0
			p.NextRound = advanceScanWake(p.NextRound, p.Status.NextAttemptAt, time.Now())
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
		scanDue := !time.Now().Before(p.NextRound)
		if sourceSweepUsesDurableRetry(kind) {
			due, dueErr := dueSourceRetryExists(ctx, s.store, kind)
			if dueErr != nil {
				s.backgroundEvent(kind, "retry_index_read_failed", dueErr)
				continue
			}
			retryTurn++
			if due && (!scanDue || retryTurn%4 == 0 || time.Since(lastRetryDispatch) >= time.Second) {
				bounded, release, ok := s.beginSourceSweepUnit(ctx, kind)
				if !ok {
					continue
				}
				guarded := inventory.WithWriteGuard(bounded, func(check context.Context) error { return lease.NewManager(s.store).Validate(check, holder) })
				processed, retryErr := s.dispatchDueSourceSweepRetry(guarded, holder, kind, &p, step)
				lastRetryDispatch = time.Now()
				if retryErr != nil {
					s.backgroundEvent(kind, "retry_dispatch_failed", retryErr)
				}
				var checkpointErr error
				if processed {
					statusCtx, statusCancel := s.backgroundCheckpoint(ctx, kind)
					if lastRetryStatus.IsZero() || time.Since(lastRetryStatus) >= 5*time.Second {
						if err := s.updateSourceRetryStatus(statusCtx, kind, &p); err != nil {
							checkpointErr = err
						} else {
							lastRetryStatus = time.Now()
						}
					}
					if checkpointErr == nil {
						markSourceRetryRoundSuccess(&p)
						p.Status.CheckpointAt = time.Now().UTC()
						checkpointErr = s.saveSourceSweep(statusCtx, holder, kind, p)
					}
					statusCancel()
				}
				release()
				if checkpointErr != nil {
					s.backgroundEvent(kind, "checkpoint_failed", checkpointErr)
					return
				}
				if processed {
					continue
				}
			}
		}
		if !scanDue {
			continue
		}
		if kind == "diagnostic_retention_events" && p.EventRoundEndDate == "" {
			partitionCtx, partitionCancel := context.WithTimeout(ctx, 5*time.Second)
			partitionCtx = inventory.WithWriteGuard(partitionCtx, func(check context.Context) error {
				return lease.NewManager(s.store).Validate(check, holder)
			})
			if err := s.initializeDiagnosticEventRound(partitionCtx, prefix, &p, time.Now().UTC()); err != nil {
				partitionCancel()
				s.backgroundEvent(kind, "partition_read_failed", err)
				continue
			}
			partitionCancel()
		}
		scanPrefix := prefix
		if kind == "diagnostic_retention_events" && p.EventPartitionDate != "" {
			scanPrefix = prefix + p.EventPartitionDate + "/"
		}
		bounded, release, ok := s.beginSourceSweepUnit(ctx, kind)
		if !ok {
			continue
		}
		guarded := inventory.WithWriteGuard(bounded, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
		p.Status.LastHeartbeatAt = time.Now().UTC()
		p.Status.Complete = false
		canScan := true
		if err := importSourceSweepBackoff(guarded, s, kind, &p, 16); err != nil {
			s.failSourceScan(guarded, kind, scanPrefix, &p, err)
			canScan = false
		}
		if err := s.repairSourceRetryIndexes(guarded, kind, &p, 16); err != nil {
			s.failSourceScan(guarded, kind, scanPrefix, &p, err)
			canScan = false
		}
		if !canScan {
			// Retry records are committed before the source cursor can advance.
		} else if recoveryWriter(kind) && s.recoveryBlocked() {
			s.sourceDependencyWait(guarded, kind, &p, errRecoveryPaused)
		} else {
			if p.Status.WaitReason == "protection_recovery" {
				p.Status.WaitReason, p.Status.DependencyGeneration = "", ""
			}
			s.sourceSweepBatch(guarded, kind, scanPrefix, &p, step)
		}
		if p.Cursor.Done && p.Status.WaitReason == "" && sourceBudgetAvailable(guarded) {
			if kind == "diagnostic_retention_events" && p.EventPartitionDate != "none" && p.EventPartitionDate < p.EventRoundEndDate {
				date, parseErr := time.Parse("20060102", p.EventPartitionDate)
				if parseErr != nil {
					s.failSourceScan(guarded, kind, scanPrefix, &p, parseErr)
				} else {
					p.EventPartitionDate = date.Add(24 * time.Hour).Format("20060102")
					p.Cursor = inventory.Cursor{}
					p.NextRound = time.Now().UTC()
					p.Status.LastCursorProgressAt = p.NextRound
					p.Status.Processed, p.Status.NextScanAt = p.Cursor.Entities, p.NextRound
					checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, kind)
					p.Status.CheckpointAt = time.Now().UTC()
					checkpointErr := s.saveSourceSweep(checkpointCtx, holder, kind, p)
					checkpointCancel()
					release()
					if checkpointErr != nil {
						s.backgroundEvent(kind, "checkpoint_failed", checkpointErr)
						return
					}
					continue
				}
			}
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
		if sourceSweepUsesDurableRetry(kind) {
			if lastRetryStatus.IsZero() || time.Since(lastRetryStatus) >= 5*time.Second {
				if err := s.updateSourceRetryStatus(guarded, kind, &p); err != nil {
					s.failSourceScan(guarded, kind, prefix, &p, err)
				} else {
					lastRetryStatus = time.Now()
				}
			}
			markSourceRetryRoundSuccess(&p)
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
