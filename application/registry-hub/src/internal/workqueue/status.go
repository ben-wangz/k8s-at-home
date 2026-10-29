package workqueue

import "time"

func NewStatus(at time.Time) Status {
	return Status{ObservedAt: at, Counts: map[string]int64{"pending": 0, "running": 0, "retry_wait": 0, "failed": 0, "done": 0}}
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
}

func (s *Status) Add(job Job) {
	if s.Counts == nil {
		s.Counts = map[string]int64{}
	}
	s.Counts[job.State]++
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
