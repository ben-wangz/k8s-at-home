package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

type backgroundStep func(context.Context, *workqueue.Job) (any, bool, error)

func (s *Service) runBackgroundQueue(ctx context.Context, kind string, step backgroundStep) {
	q := workqueue.Queue{Store: s.store}
	cursor := inventory.Cursor{}
	status := workqueue.NewStatus(time.Now().UTC())
	lastSave := time.Time{}
	scanFailed := false
	nextRound := time.Time{}
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
		unitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		release, admitted := s.backgroundBudget(unitCtx)
		if !admitted {
			cancel()
			continue
		}
		entry, ok, err := cursor.Peek(unitCtx, s.store, workqueue.Prefix+"jobs/"+kind+"/", 32)
		if err == nil && ok {
			id := strings.TrimSuffix(entry.Key[strings.LastIndex(entry.Key, "/")+1:], ".json")
			var job workqueue.Job
			job, err = q.Read(unitCtx, kind, id)
			if err == nil {
				if job.State == "done" {
					err = q.Archive(unitCtx, job)
				} else if claimed, claimErr := q.Claim(unitCtx, job); claimErr == nil {
					job = claimed
					unitCtx = context.WithValue(unitCtx, storageOperationIDKey{}, job.ID)
					started := time.Now()
					if s.metrics != nil && claimed.Fence == 1 {
						s.metrics.backgroundWait.WithLabelValues(kind).Observe(max(0, time.Since(job.CreatedAt).Seconds()))
					}
					guarded := inventory.WithWriteGuard(unitCtx, func(ctx context.Context) error { return q.Validate(ctx, claimed) })
					progress, done, processErr := step(guarded, &job)
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
					finishCtx, finishCancel := context.WithTimeout(ctx, 5*time.Second)
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
			status.ErrorAt, status.ErrorClass = time.Now().UTC(), "storage"
			if ok {
				status.ErrorEntity = entry.Key
			}
			status.Complete = false
		}
		if cursor.Done || time.Since(lastSave) > 5*time.Second {
			status.Complete = cursor.Done && !scanFailed
			err = inventory.Update(unitCtx, s.store, workqueue.Prefix+"status/"+kind+".json", func(old *workqueue.Status) error {
				if old.ErrorAt.After(status.ErrorAt) {
					status.ErrorAt, status.ErrorClass, status.ErrorEntity, status.ErrorRecovered = old.ErrorAt, old.ErrorClass, old.ErrorEntity, old.ErrorRecovered
					status.ErrorPhase = old.ErrorPhase
					if previous, readErr := q.Read(unitCtx, kind, old.ErrorEntity); readErr == nil {
						status.ErrorRecovered = previous.State == "done"
					}
				}
				*old = status
				return nil
			})
			lastSave = time.Now()
			if err == nil && cursor.Done {
				if kind == "inventory" {
					s.projectionBacklog.Store(status.Counts["pending"]+status.Counts["running"]+status.Counts["retry_wait"]+status.Counts["failed"] >= 10000)
				}
				cursor, status = inventory.Cursor{}, workqueue.NewStatus(time.Now().UTC())
				scanFailed = false
				nextRound = time.Now().Add(time.Second)
			}
		}
		release()
		cancel()
	}
}

func (s *Service) backgroundBudget(ctx context.Context) (func(), bool) {
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
}

func (s *Service) enqueueProjection(ctx context.Context, kind, source string, payload any, at time.Time) error {
	if kind == "inventory" && s.projectionBacklog.Load() {
		s.backgroundEvent(kind, "backpressure", storage.ErrUnavailable)
		return storage.ErrUnavailable
	}
	queueCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := (workqueue.Queue{Store: s.store}).Enqueue(queueCtx, kind, source, payload, at)
	if err != nil {
		s.inventoryDirty.Store(true)
		s.backgroundEvent(kind, "enqueue_failed", err)
	}
	return err
}
