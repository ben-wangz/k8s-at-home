package app

import (
	"context"
	"registry-hub/internal/workqueue"
	"time"
)

func pureContextBudget(err error) bool {
	if err == context.DeadlineExceeded || err == context.Canceled {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			if !pureContextBudget(child) {
				return false
			}
			found = true
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return pureContextBudget(wrapped.Unwrap())
	}
	return false
}

func (s *Service) recoveryBlocked() bool {
	return s.gate != nil && (s.recoveryStarting.Load() || s.gate.State().RecoveryBarrier || s.gate.recovery != nil && s.gate.recovery.blocked.Load())
}

func recoveryWriter(kind string) bool {
	switch kind {
	case "notify", "wake_root", "terminal_cleanup", "quota_wake", "download_cleanup", "cache_verification", "distribution_verification", "activation", "activation_events", "task_reconciliation", "quota_waiters", "cache_cleanup", "reader_cleanup", "task_cleanup", "download_expiry", "download_quota":
		return true
	}
	return false
}

func (s *Service) recoveryDependency() *workqueue.DependencyWait {
	generation := s.gate.State().RecoveryID
	return &workqueue.DependencyWait{Reason: "protection_recovery", Generation: generation, WorkID: generation, WorkKind: "protection_recovery", RecheckAt: time.Now().UTC().Add(5 * time.Second)}
}

func pureRecoveryPause(err error) bool {
	if err == nil {
		return false
	}
	if err == errRecoveryPaused || err == ErrRecoveryBarrier {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			if !pureRecoveryPause(child) {
				return false
			}
			found = true
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return pureRecoveryPause(wrapped.Unwrap())
	}
	return false
}

func (s *Service) dependencyResult(err error) error {
	if pureRecoveryPause(err) {
		return s.recoveryDependency()
	}
	return err
}

func (s *Service) sourceDependencyWait(ctx context.Context, kind string, p *sourceSweepCheckpoint, err error) bool {
	if !pureRecoveryPause(err) {
		return false
	}
	wait := s.recoveryDependency()
	p.Status.WaitReason, p.Status.DependencyGeneration = wait.Reason, wait.Generation
	p.Status.DependencyCheckedAt, p.NextRound = time.Now().UTC(), wait.RecheckAt
	return true
}

func (s *Service) prepareQueueDependency(ctx context.Context, q workqueue.Queue, job *workqueue.Job) (bool, error) {
	if recoveryWriter(job.Kind) && s.recoveryBlocked() && job.State != "done" && job.State != "failed" {
		return true, q.Wait(ctx, job, s.recoveryDependency())
	}
	if job.WaitReason == "protection_recovery" {
		return false, q.ResumeDependency(ctx, job)
	}
	return false, nil
}

func (s *Service) observeQueueDependency(kind string) {
	if !recoveryWriter(kind) {
		return
	}
	s.updateWorker(kind, func(state *workerStatus) {
		state.WaitReason, state.DependencyGeneration = "", ""
		if s.recoveryBlocked() {
			wait := s.recoveryDependency()
			state.State, state.WaitReason, state.DependencyGeneration = "waiting_dependency", wait.Reason, wait.Generation
			state.DependencyCheckedAt = time.Now().UTC()
		}
	})
}
