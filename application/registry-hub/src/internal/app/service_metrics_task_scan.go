package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
)

type taskMetricTotals struct {
	Counts                                                             map[tasks.State]int `json:"counts"`
	AuthorityCounts, ErrorCounts, KindCounts, WaitCounts, CancelCounts map[string]int
	RetryCounts                                                        map[string]float64
	Runnable, Waiting, RecoveryUnknown                                 float64
}

func newTaskMetricTotals() *taskMetricTotals {
	return &taskMetricTotals{Counts: map[tasks.State]int{}, AuthorityCounts: map[string]int{}, ErrorCounts: map[string]int{}, KindCounts: map[string]int{}, WaitCounts: map[string]int{}, CancelCounts: map[string]int{}, RetryCounts: map[string]float64{}}
}

func (t *taskMetricTotals) add(item tasks.Task) {
	t.Counts[item.Status]++
	t.AuthorityCounts[string(item.Status)+"\x00"+taskAuthorityClass(item.Authority)]++
	kind := stableTaskKind(item.Kind)
	key := kind + "\x00" + string(item.Status)
	t.KindCounts[key]++
	t.RetryCounts[key] += float64(item.RetryCount)
	switch item.Status {
	case tasks.Pending, tasks.Running, tasks.RetryWait:
		if item.Phase == "solidification" {
			t.Runnable++
		}
	case tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal:
		t.Waiting++
		t.WaitCounts[stableTaskWaitReason(item.WaitReason)]++
	}
	if item.CancelRequested {
		t.CancelCounts[stableTaskCancelPhase(item.CancelPhase)]++
	}
	if item.RecoveryReason != "" {
		t.RecoveryUnknown++
	}
	if item.ErrorClass != "" {
		t.ErrorCounts[stableTaskErrorClass(item.ErrorClass)]++
	}
}

func (s *Service) publishTaskMetricTotals(t *taskMetricTotals) {
	s.metrics.taskKinds.Reset()
	s.metrics.taskAuthorities.Reset()
	s.metrics.taskErrors.Reset()
	s.metrics.taskWaitReasons.Reset()
	s.metrics.taskRetries.Reset()
	s.metrics.taskCancelPhases.Reset()
	for _, state := range []tasks.State{tasks.Pending, tasks.Running, tasks.RetryWait, tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal, tasks.Solidified, tasks.Failed, tasks.Cancelled, tasks.RepairPending} {
		s.metrics.tasks.WithLabelValues(string(state)).Set(float64(t.Counts[state]))
		for _, kind := range []string{"root", "blob", "other"} {
			key := kind + "\x00" + string(state)
			s.metrics.taskKinds.WithLabelValues(kind, string(state)).Set(float64(t.KindCounts[key]))
			s.metrics.taskRetries.WithLabelValues(kind, string(state)).Set(t.RetryCounts[key])
		}
		for _, authority := range []string{"docker", "public", "local"} {
			s.metrics.taskAuthorities.WithLabelValues(string(state), authority).Set(float64(t.AuthorityCounts[string(state)+"\x00"+authority]))
		}
	}
	for _, reason := range taskWaitReasonLabels {
		s.metrics.taskWaitReasons.WithLabelValues(reason).Set(float64(t.WaitCounts[reason]))
	}
	for _, phase := range []string{"stop_requested", "quiesced", "other"} {
		s.metrics.taskCancelPhases.WithLabelValues(phase).Set(float64(t.CancelCounts[phase]))
	}
	for _, class := range []string{"upstream", "manifest_invalid", "storage", "referrer_index", "lease", "other"} {
		s.metrics.taskErrors.WithLabelValues(class).Set(float64(t.ErrorCounts[class]))
	}
	s.metrics.taskRunnable.Set(t.Runnable)
	s.metrics.taskWaiting.Set(t.Waiting)
	s.metrics.recoveryUnknown.Set(t.RecoveryUnknown)
}

func (s *Service) runTaskMetrics(ctx context.Context) {
	cursor, totals := inventory.Cursor{}, newTaskMetricTotals()
	s.runMetricSweep(ctx, "tasks", &cursor, accumulatorFor(&totals), func(ctx context.Context) (bool, error) {
		entry, ok, err := cursor.Peek(ctx, s.store, "tasks/state/", 32)
		if err != nil {
			return false, err
		}
		if ok {
			item, _, err := inventory.Read[tasks.Task](ctx, s.store, entry.Key)
			if err != nil {
				return false, err
			}
			totals.add(item)
			cursor.Advance()
		}
		return cursor.Done, nil
	}, func() { s.publishTaskMetricTotals(totals) }, func() { cursor = inventory.Cursor{}; totals = newTaskMetricTotals() }, func() map[string]int64 {
		result := map[string]int64{}
		for state, value := range totals.Counts {
			result[string(state)] = int64(value)
		}
		return result
	})
}
