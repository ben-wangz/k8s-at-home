package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"time"
)

type quotaWaitView struct {
	Category             string     `json:"category"`
	Coverage             string     `json:"coverage"`
	OriginalAgeSeconds   float64    `json:"originalAgeSeconds"`
	ExecutableAgeSeconds *float64   `json:"executableAgeSeconds"`
	EligibleAt           *time.Time `json:"eligibleAt"`
	PredecessorID        string     `json:"predecessorID,omitempty"`
	SessionID            string     `json:"sessionID,omitempty"`
	ResponsibleWorkURL   string     `json:"responsibleWorkURL,omitempty"`
	ObservedAt           time.Time  `json:"observedAt"`
}

func (s *Service) quotaWaitDiagnostic(ctx context.Context, task tasks.Task) *quotaWaitView {
	if task.QuotaWaitAt.IsZero() && task.Status != tasks.WaitingQuota {
		return nil
	}
	start := task.QuotaWaitAt
	if start.IsZero() {
		start = task.CreatedAt
	}
	v := &quotaWaitView{Category: "unknown", Coverage: "not_evaluated", OriginalAgeSeconds: max(0, time.Since(start).Seconds()), ObservedAt: time.Now().UTC(), SessionID: task.ProtectionSessionID}
	if !task.ExecutableAt.IsZero() {
		age := max(0, time.Since(task.ExecutableAt).Seconds())
		v.ExecutableAgeSeconds, v.EligibleAt = &age, &task.ExecutableAt
	}
	if s.gate == nil {
		return v
	}
	if s.recoveryBlocked() {
		v.Category, v.Coverage, v.ResponsibleWorkURL = "recovery", "current_gate", "/api/v1/workers/protection_recovery"
		return v
	}
	reason := task.WaitReason
	if reason == "" {
		reason = task.QuotaWaitReason
	}
	if reason == "distribution_quota" {
		covered, err := s.coveredDistributionBlob(ctx, task)
		if err != nil {
			v.Coverage = "read_unavailable"
			return v
		}
		if covered {
			v.Category, v.Coverage, v.ResponsibleWorkURL = "covered_scheduling", "current_session_and_cache", "/api/v1/workers/quota_wake"
			return v
		}
	}
	fits, fitErr := s.quotaWaiterFits(ctx, reason, task, s.gate.State())
	if fitErr != nil {
		v.Coverage = "read_unavailable"
		return v
	}
	if !fits {
		v.Category, v.Coverage = "capacity", "current_gate_and_demand"
		return v
	}
	p, _, err := inventory.Read[sourceSweepCheckpoint](ctx, s.store, sourceSweepKey("quota_waiters"))
	if err != nil || !p.Status.Complete || time.Since(p.Status.LastSuccessAt) > 90*time.Second {
		return v
	}
	predecessor := p.QuotaLastCandidates[reason]
	if predecessor.ID != "" && predecessor.ID != task.ID && quotaCandidateBefore(predecessor, task) {
		current, err := s.tasks.Get(ctx, predecessor.ID)
		if err == nil && !current.CancelRequested && !current.QuotaWaitAt.IsZero() && current.QuotaWaitReason == task.QuotaWaitReason {
			v.Category, v.Coverage, v.PredecessorID = "queue_predecessor", "last_complete_scan_and_current_task", current.ID
			v.ResponsibleWorkURL = "/api/v1/tasks/" + current.ID
			return v
		}
	}
	v.Category, v.Coverage = "capacity_available_scheduling", "last_complete_scan_and_current_capacity; admission_rechecks_fifo"
	return v
}
