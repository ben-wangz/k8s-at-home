package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
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
	AgeSeconds           *int64               `json:"ageSeconds"`
	ScheduleOverdue      bool                 `json:"scheduleOverdue"`
	Failed               bool                 `json:"failed"`
	Complete             bool                 `json:"complete"`
	Counts               map[string]int64     `json:"counts,omitempty"`
	RunID                string               `json:"runID,omitempty"`
	Owner                string               `json:"owner,omitempty"`
	Fence                uint64               `json:"fence,omitempty"`
	CheckpointAt         time.Time            `json:"checkpointAt,omitempty"`
	Processed            uint64               `json:"processed"`
	Resumability         string               `json:"resumability,omitempty"`
	WaitReason           string               `json:"waitReason,omitempty"`
	DependencyGeneration string               `json:"dependencyGeneration,omitempty"`
	DependencyCheckedAt  time.Time            `json:"dependencyCheckedAt,omitempty"`
}

type entityBackoff struct {
	Retries int       `json:"retries"`
	Next    time.Time `json:"next"`
	Round   uint64    `json:"round"`
}

type sourceSweepStep func(context.Context, storage.Entry, *sourceSweepCheckpoint) error
type sourceSweepEnd func(context.Context, *sourceSweepCheckpoint) error

func (s *Service) runSourceSweep(ctx context.Context, kind, prefix string, step func(context.Context, storage.Entry) error) {
	s.runSourceSweepWithState(ctx, kind, prefix, func(ctx context.Context, entry storage.Entry, _ *sourceSweepCheckpoint) error {
		return step(ctx, entry)
	}, nil)
}

func (s *Service) runSourceSweepWithState(ctx context.Context, kind, prefix string, step sourceSweepStep, end sourceSweepEnd) {
	manager := lease.NewManager(s.store)
	for ctx.Err() == nil {
		leaderCtx, holder, release, err := s.acquireBackgroundLease(ctx, kind, manager, "background/source/"+kind, s.instanceID+"/"+requestID(), time.Minute)
		if err == nil {
			s.runSourceSweepStateLeader(leaderCtx, holder, kind, prefix, step, end)
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
	s.runSourceSweepStateLeader(ctx, holder, kind, prefix, func(ctx context.Context, entry storage.Entry, _ *sourceSweepCheckpoint) error {
		return step(ctx, entry)
	}, nil)
}
