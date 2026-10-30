package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) runMetricSweep(ctx context.Context, kind string, cursor *inventory.Cursor, accumulator *scanAccumulator, step func(context.Context) (bool, error), publish, reset func(), summary ...func() map[string]int64) {
	for ctx.Err() == nil {
		leaderCtx, holder, release, err := s.acquireBackgroundLease(ctx, "statistics_"+kind, lease.NewManager(s.store), "background/statistics/"+kind, s.instanceID+"/"+requestID(), time.Minute)
		if err == nil {
			reset()
			s.runMetricSweepLeader(leaderCtx, holder, kind, cursor, accumulator, step, publish, reset, summary...)
			_ = release()
		} else if !errors.Is(err, lease.ErrHeld) && !errors.Is(err, errBackgroundAdmissionYield) {
			s.backgroundEvent("statistics_"+kind, "lease_failed", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) runMetricSweepLeader(ctx context.Context, holder lease.Lease, kind string, cursor *inventory.Cursor, accumulator *scanAccumulator, step func(context.Context) (bool, error), publish, reset func(), summary ...func() map[string]int64) {
	state := metricSweepCheckpoint{Status: backgroundScanStatus{StartedAt: time.Now().UTC(), RunID: requestID()}}
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	previous, _, readErr := inventory.Read[metricSweepCheckpoint](initial, s.store, metricCheckpointKey(kind))
	initialCancel()
	if readErr == nil && accumulator != nil {
		state = previous
		if len(state.LastComplete) > 0 {
			if err := accumulator.Restore(state.LastComplete); err != nil {
				s.backgroundEvent("statistics_"+kind, "checkpoint_corrupt", err)
				return
			}
			publish()
		}
		if err := accumulator.Restore(state.Accumulator); err != nil {
			s.backgroundEvent("statistics_"+kind, "checkpoint_corrupt", err)
			return
		}
		*cursor = state.Cursor
	} else if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
		s.backgroundEvent("statistics_"+kind, "checkpoint_read_failed", readErr)
		return
	}
	status := &state.Status
	status.Resumability, status.Owner, status.Fence = "durable_checkpoint", holder.Owner, holder.FencingToken
	if accumulator == nil {
		status.Resumability = "full_rescan_on_restart"
	}
	lastSave := time.Time{}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(state.NextRound) {
			continue
		}
		bounded, release, ok := s.beginBackgroundUnit(ctx, "statistics_"+kind, 10*time.Second)
		if !ok {
			continue
		}
		status.LastHeartbeatAt = time.Now().UTC()
		done, err := false, error(nil)
		entities := cursor.Entities
		quantum := time.Now().Add(time.Second)
		for unit := 0; unit < 4; unit++ {
			if unit > 0 && time.Now().After(quantum) {
				break
			}
			if deadline, ok := bounded.Deadline(); ok && time.Until(deadline) < time.Second {
				break
			}
			done, err = step(bounded)
			if done || err != nil {
				break
			}
		}
		if err != nil {
			state.Invalid = true
			status.Complete = false
			status.ErrorAt, status.ErrorClass, status.ErrorRecovered = time.Now().UTC(), diagnosticReason(err), false
			if cursor.Offset < len(cursor.Entries) {
				status.ErrorEntity = cursor.Entries[cursor.Offset].Key
				cursor.Advance()
				done = cursor.Done
			}
			status.NextAttemptAt = time.Now().UTC().Add(5 * time.Second)
			state.NextRound = status.NextAttemptAt
			s.recordScanFailure(bounded, "statistics_"+kind, "statistics_scan", status, &state.DiagnosticOutbox, err, 0)
			s.backgroundEvent("statistics_"+kind, "scan_failed", err)
		} else if cursor.Entities > entities {
			status.ProgressAt = time.Now().UTC()
			status.LastEntityProgressAt = status.ProgressAt
		}
		if done {
			status.Processed = cursor.Entities
			status.LastScanCompletedAt = time.Now().UTC()
			if !state.Invalid {
				recordScanRecovery("statistics_"+kind, "statistics_scan", status, &state.DiagnosticOutbox, "")
				if len(summary) > 0 {
					status.Counts = summary[0]()
				}
				publish()
				if accumulator != nil {
					state.LastComplete, _ = json.Marshal(accumulator.Capture())
				}
				status.ObservedAt, status.Complete, status.ErrorRecovered = status.StartedAt, true, !status.ErrorAt.IsZero()
				status.LastSuccessAt = status.LastScanCompletedAt
			}
			if state.Invalid {
				status.Complete = false
			}
			reset()
			state.Invalid = false
			status.StartedAt = time.Now().UTC()
			status.RunID = requestID()
			status.NextAttemptAt = time.Time{}
			state.NextRound = time.Now().Add(15 * time.Second)
		}
		status.NextScanAt = state.NextRound
		if done || time.Since(lastSave) > 5*time.Second {
			checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, "statistics_"+kind)
			status.CheckpointAt = time.Now().UTC()
			state.Cursor = *cursor
			var checkpointErr error
			if accumulator != nil {
				state.Accumulator, checkpointErr = json.Marshal(accumulator.Capture())
			}
			if checkpointErr == nil {
				checkpointErr = s.saveMetricSweep(checkpointCtx, holder, kind, state)
			}
			checkpointCancel()
			if checkpointErr != nil {
				release()
				s.backgroundEvent("statistics_"+kind, "checkpoint_failed", checkpointErr)
				return
			}
			lastSave = time.Now()
		}
		release()
	}
}
