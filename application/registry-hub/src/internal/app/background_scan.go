package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type backgroundScanStatus struct {
	StartedAt            time.Time            `json:"startedAt"`
	ObservedAt           time.Time            `json:"observedAt"`
	ProgressAt           time.Time            `json:"progressAt"`
	LastHeartbeatAt      time.Time            `json:"lastHeartbeatAt"`
	LastScanCompletedAt  time.Time            `json:"lastScanCompletedAt"`
	LastSuccessAt        time.Time            `json:"lastSuccessAt"`
	LastEntityProgressAt time.Time            `json:"lastEntityProgressAt"`
	ErrorAt              time.Time            `json:"errorAt,omitempty"`
	ErrorEntity          string               `json:"errorEntity,omitempty"`
	ErrorClass           string               `json:"errorClass,omitempty"`
	NextAttemptAt        time.Time            `json:"nextAttemptAt,omitempty"`
	NextScanAt           time.Time            `json:"nextScanAt,omitempty"`
	ErrorRecovered       bool                 `json:"errorRecovered"`
	LastError            *diagnostics.Failure `json:"lastError,omitempty"`
	RecoveredAt          time.Time            `json:"recoveredAt"`
	Stale                bool                 `json:"stale"`
	Complete             bool                 `json:"complete"`
	Counts               map[string]int64     `json:"counts,omitempty"`
	RunID                string               `json:"runID,omitempty"`
	Owner                string               `json:"owner,omitempty"`
	Fence                uint64               `json:"fence,omitempty"`
	CheckpointAt         time.Time            `json:"checkpointAt,omitempty"`
	Processed            uint64               `json:"processed"`
	Resumability         string               `json:"resumability,omitempty"`
}

type entityBackoff struct {
	Retries int       `json:"retries"`
	Next    time.Time `json:"next"`
	Round   uint64    `json:"round"`
}

func (s *Service) runSourceSweep(ctx context.Context, kind, prefix string, step func(context.Context, storage.Entry) error) {
	manager := lease.NewManager(s.store)
	for ctx.Err() == nil {
		leaderCtx, holder, release, err := s.acquireBackgroundLease(ctx, kind, manager, "background/source/"+kind, s.instanceID+"/"+requestID(), time.Minute)
		if err == nil {
			s.runSourceSweepLeader(leaderCtx, holder, kind, prefix, step)
			_ = release()
		} else if !errors.Is(err, errBackgroundAdmissionYield) {
			if !errors.Is(err, lease.ErrHeld) {
				s.backgroundEvent(kind, "lease_acquisition_failed", err)
			}
			s.updateWorker(kind, func(state *workerStatus) {
				state.State = "backoff"
				if errors.Is(err, lease.ErrHeld) {
					state.State = "lease_held_elsewhere"
				}
				state.ErrorClass = diagnosticReason(err)
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (s *Service) runSourceSweepLeader(ctx context.Context, holder lease.Lease, kind, prefix string, step func(context.Context, storage.Entry) error) {
	cursor := inventory.Cursor{}
	status := backgroundScanStatus{StartedAt: time.Now().UTC()}
	backoff := map[string]entityBackoff{}
	round, scanRetries := uint64(1), 0
	roundFailed := false
	nextRound, lastSave := time.Time{}, time.Time{}
	var outbox []diagnostics.Event
	initial, initialCancel := context.WithTimeout(ctx, 5*time.Second)
	prior, _, readErr := inventory.Read[sourceSweepCheckpoint](initial, s.store, sourceSweepKey(kind))
	initialCancel()
	if readErr == nil {
		status, cursor, backoff = prior.Status, prior.Cursor, prior.Backoff
		outbox = prior.DiagnosticOutbox
		round, roundFailed, nextRound = prior.Round, prior.RoundFailed, prior.NextRound
		scanRetries = prior.ScanRetries
		if backoff == nil {
			backoff = map[string]entityBackoff{}
		}
	} else if !errors.Is(readErr, storage.ErrNotFound) {
		s.backgroundEvent(kind, "checkpoint_read_failed", readErr)
		return
	}
	status.Owner, status.Fence, status.Resumability = holder.Owner, holder.FencingToken, "durable_checkpoint"
	if status.RunID == "" {
		status.RunID = requestID()
	}
	ticker := time.NewTicker(500 * time.Millisecond)
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
		bounded, release, ok := s.beginBackgroundUnit(ctx, kind, 15*time.Second)
		if !ok {
			continue
		}
		status.LastHeartbeatAt = time.Now().UTC()
		entry, found, err := cursor.Peek(bounded, s.store, prefix, 32)
		if err == nil {
			scanRetries = 0
		}
		if err == nil && found {
			id := entry.Key + "\x00" + entry.Version
			if b, exists := backoff[id]; exists {
				b.Round = round
				backoff[id] = b
			}
			for previous := range backoff {
				if strings.HasPrefix(previous, entry.Key+"\x00") && previous != id {
					delete(backoff, previous)
				}
			}
			if !backoff[id].Next.After(time.Now()) {
				err = step(bounded, entry)
				if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
					status.Complete = false
					b := backoff[id]
					b.Retries++
					b.Round = round
					b.Next = time.Now().Add(min(time.Second*time.Duration(1<<min(b.Retries, 8)), 5*time.Minute))
					b.Next = b.Next.Add(time.Duration(time.Now().UnixNano() % int64(time.Second)))
					if len(backoff) < 1024 || b.Retries > 1 {
						backoff[id] = b
					} else {
						nextRound = b.Next
					}
					roundFailed = true
					status.ErrorAt, status.ErrorEntity, status.ErrorClass, status.NextAttemptAt, status.ErrorRecovered = time.Now().UTC(), entry.Key, diagnosticReason(err), b.Next, false
					s.recordScanFailure(bounded, kind, "source_entity", &status, &outbox, err, uint64(b.Retries))
					s.backgroundEvent(kind, "entity_failed", err)
				} else {
					if backoff[id].Retries > 0 {
						recordScanRecovery(kind, "source_entity", &status, &outbox, entry.Key)
					}
					delete(backoff, id)
					status.ProgressAt = time.Now().UTC()
					status.LastEntityProgressAt = status.ProgressAt
					if status.ErrorEntity == entry.Key {
						status.ErrorRecovered = true
					}
				}
			}
			cursor.Advance()
		}
		if err != nil && !found {
			scanRetries++
			nextRound = time.Now().Add(min(time.Second*time.Duration(1<<min(scanRetries, 8)), 5*time.Minute))
			status.Complete, roundFailed = false, true
			status.ErrorAt, status.ErrorClass, status.ErrorEntity, status.NextAttemptAt, status.ErrorRecovered = time.Now().UTC(), diagnosticReason(err), prefix, nextRound, false
			s.recordScanFailure(bounded, kind, "source_scan", &status, &outbox, err, uint64(scanRetries))
			s.backgroundEvent(kind, "scan_failed", err)
		}
		if cursor.Done {
			for id, b := range backoff {
				if b.Round != round {
					delete(backoff, id)
				}
			}
			status.Complete, status.ObservedAt = len(backoff) == 0 && !roundFailed, time.Now().UTC()
			status.LastScanCompletedAt = status.ObservedAt
			if status.Complete {
				recordScanRecovery(kind, "source_scan", &status, &outbox, "")
				status.LastSuccessAt = status.ObservedAt
				status.ErrorRecovered = !status.ErrorAt.IsZero()
				status.NextAttemptAt = time.Time{}
			}
			round++
			roundFailed = false
			cursor = inventory.Cursor{}
			nextRound = time.Now().Add(15 * time.Second)
		}
		status.Processed, status.NextScanAt = cursor.Entities, nextRound
		if cursor.Entities == 0 || time.Since(lastSave) > 5*time.Second {
			checkpointCtx, checkpointCancel := s.backgroundCheckpoint(ctx, kind)
			status.CheckpointAt = time.Now().UTC()
			err = s.saveSourceSweep(checkpointCtx, holder, kind, sourceSweepCheckpoint{Status: status, Cursor: cursor, Backoff: backoff, Round: round, RoundFailed: roundFailed, ScanRetries: scanRetries, NextRound: nextRound, DiagnosticOutbox: outbox})
			checkpointCancel()
			if err != nil {
				release()
				s.backgroundEvent(kind, "checkpoint_failed", err)
				return
			}
			lastSave = time.Now()
		}
		s.updateWorker(kind, func(state *workerStatus) {
			state.Resumability, state.LastEntityProgressAt, state.ErrorClass = "durable_checkpoint", status.LastEntityProgressAt, status.ErrorClass
		})
		release()
	}
}
