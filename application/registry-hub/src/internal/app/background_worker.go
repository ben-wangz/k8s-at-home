package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

type backgroundStep func(context.Context, *workqueue.Job) (any, bool, error)

func (s *Service) runBackgroundQueueLeader(ctx context.Context, holder lease.Lease, kind string, step backgroundStep) {
	q := workqueue.Queue{Store: s.store}
	cursor := inventory.Cursor{}
	status := workqueue.NewStatus(time.Now().UTC())
	lastSave := time.Time{}
	scanFailed := false
	nextRound := time.Time{}
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	prior, _, readErr := inventory.Read[queueSweepCheckpoint](initial, s.store, queueCheckpointKey(kind))
	initialCancel()
	if readErr == nil {
		cursor, status, scanFailed, nextRound = prior.Cursor, prior.Status, prior.ScanFailed, prior.NextRound
	} else if !errors.Is(readErr, storage.ErrNotFound) {
		s.backgroundEvent(kind, "checkpoint_read_failed", readErr)
		return
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Now().Before(nextRound) {
			continue
		}
		unitCtx, release, admitted := s.beginBackgroundUnit(ctx, kind, 15*time.Second)
		if !admitted {
			continue
		}
		entry, ok, err := cursor.Peek(unitCtx, s.store, workqueue.Prefix+"jobs/"+kind+"/", 32)
		if err == nil && ok {
			id := strings.TrimSuffix(entry.Key[strings.LastIndex(entry.Key, "/")+1:], ".json")
			var job workqueue.Job
			job, err = q.Read(unitCtx, kind, id)
			if err == nil {
				if job.State == "running" {
					job.PreviousLeaseOwner = job.LeaseOwner
				}
				job.LeaseOwner = s.instanceID
				if timing, ok := unitCtx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
					job.RunID, job.AdmissionWaitSeconds, job.ExecutionBudgetSeconds = timing.RunID, timing.Admission, timing.Budget
				}
				if job.State == "done" {
					err = q.Archive(unitCtx, job)
				} else if claimed, claimErr := q.Claim(unitCtx, job); claimErr == nil {
					job = claimed
					if timing, ok := unitCtx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
						timing.WorkID, timing.AttemptID = job.ID, job.AttemptID
						unitCtx = context.WithValue(unitCtx, diagnosticContextKey{}, timing)
					}
					unitCtx = context.WithValue(unitCtx, storageOperationIDKey{}, job.ID)
					started := time.Now()
					if s.metrics != nil && claimed.Fence == 1 {
						if !job.EnqueuedAt.IsZero() {
							s.metrics.backgroundWait.WithLabelValues(kind).Observe(max(0, time.Since(job.EnqueuedAt).Seconds()))
						}
					}
					guarded := inventory.WithWriteGuard(unitCtx, func(ctx context.Context) error { return q.Validate(ctx, claimed) })
					progress, done, processErr := step(guarded, &job)
					job.ExecutionDurationSeconds = time.Since(started).Seconds()
					if timing, ok := unitCtx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
						job.RunID, job.AdmissionWaitSeconds, job.ExecutionBudgetSeconds = timing.RunID, timing.Admission, timing.Budget
						job.TimeoutScope = "unknown"
						if processErr != nil && unitCtx.Err() != nil {
							job.TimeoutScope = timing.Scope
						}
					}
					if s.metrics != nil {
						result := "yield"
						if done {
							result = "completed"
						}
						if processErr != nil {
							result = "error"
						}
						s.metrics.backgroundDuration.WithLabelValues(kind, result).Observe(time.Since(started).Seconds())
					}
					finishCtx, finishCancel := s.backgroundCheckpoint(ctx, kind)
					err = q.Finish(finishCtx, &job, progress, done, processErr)
					finishCancel()
					if processErr != nil {
						event := "retry"
						if job.State == "failed" {
							event = "permanent_failure"
						}
						s.backgroundEvent(kind, event, processErr)
					} else if done && err == nil {
						s.backgroundEvent(kind, "completed", nil)
						s.inventoryDirty.Store(true)
					}
				} else if !errors.Is(claimErr, storage.ErrConditionFailed) {
					err = claimErr
				}
				status.Add(job)
				if kind == "download_cleanup" && job.State != "done" {
					var payload downloadCleanupWork
					if json.Unmarshal(job.Payload, &payload) == nil {
						status.PendingBytes += payload.Bytes
					}
				}
			}
			// A corrupt entity must not prevent the rest of the queue from progressing.
			cursor.Advance()
		}
		if err != nil {
			scanFailed = true
			s.backgroundEvent(kind, "scan_failed", err)
			status.ErrorAt, status.ErrorClass = time.Now().UTC(), diagnosticReason(err)
			if ok {
				status.ErrorEntity = entry.Key
			}
			status.Complete = false
		}
		if cursor.Done || time.Since(lastSave) > 5*time.Second {
			checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, kind)
			status.Complete = cursor.Done && !scanFailed
			status.Resumability, status.LeaseOwner, status.LeaseFence, status.CheckpointAt = "durable_checkpoint", holder.Owner, holder.FencingToken, time.Now().UTC()
			if old, _, readErr := inventory.Read[workqueue.Status](checkpointCtx, s.store, workqueue.Prefix+"status/"+kind+".json"); readErr == nil && old.ErrorAt.After(status.ErrorAt) {
				status.ErrorAt, status.ErrorClass, status.ErrorEntity, status.ErrorRecovered = old.ErrorAt, old.ErrorClass, old.ErrorEntity, old.ErrorRecovered
				status.ErrorPhase = old.ErrorPhase
				if previous, readErr := q.Read(checkpointCtx, kind, old.ErrorEntity); readErr == nil {
					status.ErrorRecovered = previous.State == "done"
				}
			}
			err = s.saveQueueSweep(checkpointCtx, holder, kind, queueSweepCheckpoint{Cursor: cursor, Status: status, ScanFailed: scanFailed, NextRound: nextRound})
			checkpointCancel()
			if err != nil {
				release()
				s.backgroundEvent(kind, "checkpoint_failed", err)
				return
			}
			lastSave = time.Now()
			if err == nil && cursor.Done {
				if kind == "inventory" {
					s.projectionBacklog.Store(status.Counts["pending"]+status.Counts["running"]+status.Counts["retry_wait"]+status.Counts["waiting_dependency"]+status.Counts["failed"] >= 10000)
				}
				cursor, status = inventory.Cursor{}, workqueue.NewStatus(time.Now().UTC())
				scanFailed = false
				nextRound = time.Now().Add(time.Second)
			}
		}
		release()
	}
}

func (s *Service) backgroundBudget(ctx context.Context) (func(), bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	if s.backgroundSlots == nil {
		return func() {}, true
	}
	select {
	case s.backgroundSlots <- struct{}{}:
		if s.backgroundPace != nil {
			select {
			case <-ctx.Done():
				<-s.backgroundSlots
				return nil, false
			case <-s.backgroundPace:
			}
		}
		if ctx.Err() != nil {
			<-s.backgroundSlots
			return nil, false
		}
		return func() { <-s.backgroundSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (s *Service) backgroundEvent(kind, event string, cause error) {
	if s.metrics != nil {
		s.metrics.recordEvent("background_" + kind + "_" + event)
	}
	if cause != nil && s.logger != nil {
		s.logger.Warn("background work deferred", "work_kind", kind, "event", event, "error_class", storageResult(cause))
	}
	if cause != nil {
		s.recordWorkerObservation(kind, event, cause)
	}
}

func (s *Service) enqueueProjection(ctx context.Context, kind, source string, payload any, at time.Time) error {
	if kind == "inventory" && s.projectionBacklog.Load() {
		s.backgroundEvent(kind, "backpressure", storage.ErrUnavailable)
		return storage.ErrUnavailable
	}
	queueCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	job, err := (workqueue.Queue{Store: s.store}).Enqueue(queueCtx, kind, source, payload, at)
	if err == nil && kind == "inventory" {
		if manifest, ok := payload.(inventoryManifestWork); ok {
			if recordErr := s.recordProjectionRevision(queueCtx, &job, manifest, job.State == "done"); recordErr != nil {
				s.backgroundEvent(kind, "revision_observation_failed", recordErr)
			}
		}
	}
	if err != nil {
		s.inventoryDirty.Store(true)
		s.backgroundEvent(kind, "enqueue_failed", err)
		raw, _ := json.Marshal(payload)
		association := diagnosticJob(workqueue.Job{Kind: kind, Payload: raw})
		s.emitDiagnosticEvent(diagnostics.Event{Component: "background", Operation: "enqueue", Stage: "durable_handoff", Result: "deferred", WorkKind: kind, WorkID: workqueue.ID(kind, source), Authority: association.Authority, Repository: association.Repository, ImageID: association.ImageID, Digest: association.Digest, TaskID: association.TaskID, IntentID: association.IntentID, Reason: "source_reconciliation_required", ErrorClass: diagnosticReason(err), ErrorSummary: diagnostics.SafeErrorText(err)})
	}
	return err
}
