package workqueue

import "time"

func NewStatus(at time.Time) Status {
	return Status{ObservedAt: at, Counts: map[string]int64{"pending": 0, "running": 0, "retry_wait": 0, "waiting_dependency": 0, "failed": 0, "done": 0, "superseded": 0}}
}

type Status struct {
	Counts           map[string]int64 `json:"counts"`
	ObservedAt       time.Time        `json:"observedAt"`
	OldestAt         *time.Time       `json:"oldestAt"`
	ProgressAt       time.Time        `json:"progressAt"`
	RetryCount       int64            `json:"retryCount"`
	NextAttemptAt    *time.Time       `json:"nextAttemptAt"`
	ErrorAt          time.Time        `json:"errorAt,omitempty"`
	ErrorClass       string           `json:"errorClass,omitempty"`
	ErrorEntity      string           `json:"errorEntity,omitempty"`
	ErrorPhase       int              `json:"errorPhase"`
	ErrorRecovered   bool             `json:"errorRecovered"`
	PendingBytes     int64            `json:"pendingBytes"`
	Complete         bool             `json:"complete"`
	Stale            bool             `json:"stale"`
	OldestAgeSeconds *int64           `json:"oldestAgeSeconds"`
	OldestEnqueuedAt *time.Time       `json:"oldestEnqueuedAt"`
	OldestRunnableAt *time.Time       `json:"oldestRunnableAt"`
	Resumability     string           `json:"resumability"`
	CheckpointAt     time.Time        `json:"checkpointAt"`
	LeaseOwner       string           `json:"leaseOwner,omitempty"`
	LeaseFence       uint64           `json:"leaseFence"`
}

func (s *Status) Add(job Job) {
	if s.Counts == nil {
		s.Counts = map[string]int64{}
	}
	state := job.State
	if job.Superseded {
		state = "superseded"
	}
	if state == "pending" && job.WaitReason != "" {
		state = "waiting_dependency"
	}
	s.Counts[state]++
	if state != "done" && state != "superseded" && !job.EnqueuedAt.IsZero() && (s.OldestEnqueuedAt == nil || job.EnqueuedAt.Before(*s.OldestEnqueuedAt)) {
		at := job.EnqueuedAt
		s.OldestEnqueuedAt = &at
	}
	if (state == "pending" || state == "retry_wait" && !job.NextAttemptAt.After(s.ObservedAt)) && !job.RunnableAt.IsZero() && (s.OldestRunnableAt == nil || job.RunnableAt.Before(*s.OldestRunnableAt)) {
		at := job.RunnableAt
		s.OldestRunnableAt = &at
	}
	s.RetryCount += int64(job.RetryCount)
	if job.State != "done" && (s.OldestAt == nil || job.CreatedAt.Before(*s.OldestAt)) {
		at := job.CreatedAt
		s.OldestAt = &at
	}
	if job.ProgressAt.After(s.ProgressAt) {
		s.ProgressAt = job.ProgressAt
	}
	if !job.NextAttemptAt.IsZero() && (s.NextAttemptAt == nil || job.NextAttemptAt.Before(*s.NextAttemptAt)) {
		at := job.NextAttemptAt
		s.NextAttemptAt = &at
	}
	if job.ErrorAt.After(s.ErrorAt) {
		s.ErrorAt, s.ErrorClass, s.ErrorEntity, s.ErrorRecovered = job.ErrorAt, job.ErrorClass, job.ID, job.State == "done"
		s.ErrorPhase = job.ErrorPhase
	}
}
