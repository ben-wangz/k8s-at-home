package workqueue

import (
	"context"
	"time"
)

type DependencyWait struct {
	Reason     string
	Generation string
	WorkID     string
	WorkKind   string
	RecheckAt  time.Time
}

func (w *DependencyWait) Error() string { return "waiting for dependency: " + w.Reason }

// Dependency returns a wait only when every error leaf describes the same dependency.
func Dependency(err error) (*DependencyWait, bool) {
	if err == nil {
		return nil, false
	}
	if w, ok := err.(*DependencyWait); ok {
		return w, w.Reason != ""
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var result *DependencyWait
		for _, child := range joined.Unwrap() {
			if child == nil {
				continue
			}
			w, pure := Dependency(child)
			if !pure || result != nil && (result.Reason != w.Reason || result.Generation != w.Generation) {
				return nil, false
			}
			result = w
		}
		return result, result != nil
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return Dependency(wrapped.Unwrap())
	}
	return nil, false
}

func applyDependency(job *Job, wait *DependencyWait) {
	job.State, job.LeaseUntil, job.RunnableAt = "pending", time.Time{}, time.Time{}
	job.CompletedAt = time.Time{}
	job.WaitReason, job.DependencyGeneration = wait.Reason, wait.Generation
	job.ResponsibleWorkID, job.ResponsibleWorkKind = wait.WorkID, wait.WorkKind
	job.DependencyCheckedAt = time.Now().UTC()
	job.NextAttemptAt = wait.RecheckAt
	if job.NextAttemptAt.IsZero() {
		job.NextAttemptAt = time.Now().UTC().Add(5 * time.Second)
	}
}

func (q Queue) Wait(ctx context.Context, job *Job, wait *DependencyWait) error {
	if job.State == "done" || job.State == "failed" || job.State == "running" && job.LeaseUntil.After(time.Now()) {
		return nil
	}
	if job.State == "running" {
		job.Fence++
		job.AttemptFinishedAt = time.Now().UTC()
	}
	if job.WaitReason == wait.Reason && job.DependencyGeneration == wait.Generation && job.NextAttemptAt.After(time.Now()) {
		return nil
	}
	applyDependency(job, wait)
	return q.save(ctx, job)
}

func (q Queue) ResumeDependency(ctx context.Context, job *Job) error {
	if job.State != "pending" || job.WaitReason == "" {
		return nil
	}
	job.WaitReason, job.DependencyGeneration, job.ResponsibleWorkID, job.ResponsibleWorkKind = "", "", "", ""
	job.NextAttemptAt, job.RunnableAt = time.Time{}, time.Now().UTC()
	return q.save(ctx, job)
}
