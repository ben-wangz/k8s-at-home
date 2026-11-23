package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"time"
)

type quotaWakeWork struct {
	TaskID     string `json:"taskID,omitempty"`
	Generation uint64 `json:"generation"`
	RoundID    string `json:"roundID,omitempty"`
	Revision   uint64 `json:"revision"`
	SessionID  string `json:"sessionID,omitempty"`
}

type quotaWakeProgress struct {
	TargetChecked bool `json:"targetChecked"`
}

func (s *solidifier) TaskQuotaChanged(ctx context.Context, task tasks.Task) {
	if s.service != nil {
		_ = s.service.enqueueQuotaTransition(ctx, task)
	}
}

func (s *Service) enqueueQuotaTransition(ctx context.Context, task tasks.Task) error {
	if task.QuotaWakeRevision == 0 {
		return nil
	}
	return s.updateQuotaWakeDispatch(ctx, task)
}

func (s *Service) quotaWakeStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var work quotaWakeWork
	var p quotaWakeProgress
	if json.Unmarshal(job.Payload, &work) != nil || work.TaskID == "" || work.Revision == 0 || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &p) != nil {
		return p, false, storage.ErrCorruptObject
	}
	if work.TaskID != "" {
		task, err := s.tasks.Get(ctx, work.TaskID)
		if errors.Is(err, storage.ErrNotFound) || err == nil && (task.Generation != work.Generation || task.RoundID != work.RoundID || task.QuotaWakeRevision != work.Revision || task.ProtectionSessionID != work.SessionID) {
			job.Superseded = true
			return p, true, nil
		}
		if err != nil {
			return p, false, err
		}
		if !p.TargetChecked {
			if err := s.wakeCoveredBlob(ctx, task); err != nil {
				return p, false, err
			}
			p.TargetChecked = true
		}
	}
	err := inventory.Update(ctx, s.store, quotaSignalKey, func(signal *quotaSignal) error {
		signal.Revision++
		return nil
	})
	return p, err == nil, err
}

func sourceBudgetAvailable(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ctx.Err() == nil && (!ok || time.Until(deadline) > 2*time.Second)
}

func selectQuotaCandidate(candidates map[string]tasks.Task, task tasks.Task) {
	reason := task.WaitReason
	eligible := task.Status == tasks.WaitingQuota
	if (task.Status == tasks.Pending || task.Status == tasks.Running) && !task.QuotaWaitAt.IsZero() {
		reason, eligible = task.QuotaWaitReason, true
	}
	if eligible && !task.CancelRequested && (reason == "distribution_quota" || reason == "protected_cache_capacity") {
		if current := candidates[reason]; current.ID == "" || quotaCandidateBefore(task, current) {
			candidates[reason] = quotaCandidateSnapshot(task)
		}
	}
}

func quotaCandidateSnapshot(task tasks.Task) tasks.Task {
	return tasks.Task{ID: task.ID, Kind: task.Kind, Generation: task.Generation, RoundID: task.RoundID, Status: task.Status, WaitReason: task.WaitReason, QuotaWaitAt: task.QuotaWaitAt, QuotaWaitReason: task.QuotaWaitReason, CreatedAt: task.CreatedAt}
}
