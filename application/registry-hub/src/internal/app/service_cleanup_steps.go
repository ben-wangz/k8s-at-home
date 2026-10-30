package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/download"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) acquireDownloadCleanup(ctx context.Context, identity, _ string) (context.Context, func(), bool, error) {
	leaseCtx, objectLease, release, err := s.acquireBackgroundUnitLease(ctx, "download_cleanup", s.tasks.Leases(), "objects/"+identity, "download-cleanup-"+requestID(), objectLeaseTTL)
	if errors.Is(err, lease.ErrHeld) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	guarded := download.WithCleanupGuard(leaseCtx, func(ctx context.Context) error { return s.tasks.Leases().Validate(ctx, objectLease) })
	return guarded, func() { _ = release() }, true, nil
}

func (s *Service) runDownloadQuotaCleanup(ctx context.Context) {
	last := ""
	s.runPeriodicMaintenance(ctx, "download_quota", func(ctx context.Context) (maintenanceProgress, error) {
		before := last
		err := s.downloadManager().CleanupOrphanReservationStep(ctx, &last, s.acquireDownloadCleanup)
		return maintenanceProgress{EntityProgress: last != before, RoundComplete: last == ""}, err
	}, &last)
}

func (s *Service) runStorageOrphanCleanup(ctx context.Context) {
	cleaner, ok := s.store.(interface {
		CleanupOrphansStep(context.Context, time.Duration, string) (string, int, error)
	})
	if !ok {
		return
	}
	token := ""
	s.runPeriodicMaintenance(ctx, "storage_orphans", func(ctx context.Context) (maintenanceProgress, error) {
		next, removed, err := cleaner.CleanupOrphansStep(ctx, time.Hour, token)
		progress := next != token || removed > 0
		if err == nil {
			token = next
		}
		return maintenanceProgress{EntityProgress: progress, RoundComplete: next == ""}, err
	}, &token)
}

type maintenanceProgress struct {
	EntityProgress bool
	RoundComplete  bool
}

func (s *Service) runPeriodicMaintenance(ctx context.Context, kind string, step func(context.Context) (maintenanceProgress, error), cursor ...any) {
	for ctx.Err() == nil {
		leaderCtx, holder, release, err := s.acquireBackgroundLease(ctx, kind, lease.NewManager(s.store), "background/maintenance/"+kind, s.instanceID+"/"+requestID(), time.Minute)
		if err == nil {
			s.runMaintenanceLeader(leaderCtx, holder, kind, step, cursor...)
			_ = release()
		} else if !errors.Is(err, lease.ErrHeld) && !errors.Is(err, errBackgroundAdmissionYield) {
			s.backgroundEvent(kind, "lease_failed", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) runMaintenanceLeader(ctx context.Context, holder lease.Lease, kind string, step func(context.Context) (maintenanceProgress, error), cursor ...any) {
	interval := 5 * time.Second
	if kind == "storage_inventory" {
		interval = 250 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	state := maintenanceCheckpoint{Status: backgroundScanStatus{StartedAt: time.Now().UTC(), RunID: requestID()}, Delay: time.Second}
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	prior, _, readErr := inventory.Read[maintenanceCheckpoint](initial, s.store, maintenanceCheckpointKey(kind))
	initialCancel()
	if readErr == nil {
		state = prior
		if len(cursor) > 0 && json.Unmarshal(state.Cursor, cursor[0]) != nil {
			s.backgroundEvent(kind, "checkpoint_corrupt", storage.ErrCorruptObject)
			return
		}
	} else if !errors.Is(readErr, storage.ErrNotFound) {
		s.backgroundEvent(kind, "checkpoint_read_failed", readErr)
		return
	}
	state.Delay = max(state.Delay, time.Second)
	status := &state.Status
	status.Owner, status.Fence, status.Resumability = holder.Owner, holder.FencingToken, "full_rescan_on_restart"
	if kind == "storage_inventory" || len(cursor) > 0 {
		status.Resumability = "durable_checkpoint"
	}
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(state.Next) {
			continue
		}
		bounded, release, ok := s.beginBackgroundUnit(ctx, kind, 15*time.Second)
		if ok {
			status.LastHeartbeatAt = time.Now().UTC()
			progress, err := step(bounded)
			if err != nil {
				s.backgroundEvent(kind, "deferred", err)
				state.Next = time.Now().Add(state.Delay + time.Duration(time.Now().UnixNano()%int64(state.Delay/4+1)))
				state.Delay = min(state.Delay*2, 5*time.Minute)
				status.ErrorAt, status.ErrorClass, status.ErrorRecovered, status.Complete, status.NextAttemptAt = time.Now().UTC(), diagnosticReason(err), false, false, state.Next
				s.recordScanFailure(bounded, kind, "maintenance", status, &state.DiagnosticOutbox, err, 0)
			} else {
				state.Delay = time.Second
				status.NextAttemptAt = time.Time{}
				if progress.EntityProgress {
					status.ProgressAt = time.Now().UTC()
					status.LastEntityProgressAt = status.ProgressAt
					status.Processed++
				}
				if progress.EntityProgress {
					status.Complete = false
				}
				if progress.RoundComplete {
					recordScanRecovery(kind, "maintenance", status, &state.DiagnosticOutbox, "")
					status.Complete = true
					status.ObservedAt = time.Now().UTC()
					status.LastScanCompletedAt = status.ObservedAt
					status.LastSuccessAt = status.ObservedAt
					status.ErrorRecovered = !status.ErrorAt.IsZero()
				}
			}
			status.NextScanAt = time.Now().Add(interval)
			if !progress.EntityProgress && !progress.RoundComplete && err == nil {
				release()
				continue
			}
			checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, kind)
			status.CheckpointAt = time.Now().UTC()
			var checkpointErr error
			if len(cursor) > 0 {
				state.Cursor, checkpointErr = json.Marshal(cursor[0])
			}
			if checkpointErr == nil {
				checkpointErr = s.saveMaintenanceCheckpoint(checkpointCtx, holder, kind, state)
			}
			checkpointCancel()
			release()
			if checkpointErr != nil {
				s.backgroundEvent(kind, "checkpoint_failed", checkpointErr)
				return
			}
		}
	}
}
