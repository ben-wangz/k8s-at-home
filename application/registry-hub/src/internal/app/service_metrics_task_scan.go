package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
)

type taskMetricTotals struct {
	counts                                                             map[tasks.State]int
	authorityCounts, errorCounts, kindCounts, waitCounts, cancelCounts map[string]int
	retryCounts                                                        map[string]float64
	runnable, waiting, recoveryUnknown                                 float64
}

func newTaskMetricTotals() *taskMetricTotals {
	return &taskMetricTotals{counts: map[tasks.State]int{}, authorityCounts: map[string]int{}, errorCounts: map[string]int{}, kindCounts: map[string]int{}, waitCounts: map[string]int{}, cancelCounts: map[string]int{}, retryCounts: map[string]float64{}}
}

func (t *taskMetricTotals) add(item tasks.Task) {
	t.counts[item.Status]++
	t.authorityCounts[string(item.Status)+"\x00"+taskAuthorityClass(item.Authority)]++
	kind := stableTaskKind(item.Kind)
	key := kind + "\x00" + string(item.Status)
	t.kindCounts[key]++
	t.retryCounts[key] += float64(item.RetryCount)
	switch item.Status {
	case tasks.Pending, tasks.Running, tasks.RetryWait:
		if item.Phase == "solidification" {
			t.runnable++
		}
	case tasks.WaitingCache, tasks.WaitingDeps, tasks.WaitingQuota, tasks.VerifiedLocal:
		t.waiting++
		t.waitCounts[stableTaskWaitReason(item.WaitReason)]++
	}
	if item.CancelRequested {
		t.cancelCounts[stableTaskCancelPhase(item.CancelPhase)]++
	}
	if item.RecoveryReason != "" {
		t.recoveryUnknown++
	}
	if item.ErrorClass != "" {
		t.errorCounts[stableTaskErrorClass(item.ErrorClass)]++
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
		s.metrics.tasks.WithLabelValues(string(state)).Set(float64(t.counts[state]))
		for _, kind := range []string{"root", "blob", "other"} {
			key := kind + "\x00" + string(state)
			s.metrics.taskKinds.WithLabelValues(kind, string(state)).Set(float64(t.kindCounts[key]))
			s.metrics.taskRetries.WithLabelValues(kind, string(state)).Set(t.retryCounts[key])
		}
		for _, authority := range []string{"docker", "public", "local"} {
			s.metrics.taskAuthorities.WithLabelValues(string(state), authority).Set(float64(t.authorityCounts[string(state)+"\x00"+authority]))
		}
	}
	for _, reason := range taskWaitReasonLabels {
		s.metrics.taskWaitReasons.WithLabelValues(reason).Set(float64(t.waitCounts[reason]))
	}
	for _, phase := range []string{"stop_requested", "quiesced", "other"} {
		s.metrics.taskCancelPhases.WithLabelValues(phase).Set(float64(t.cancelCounts[phase]))
	}
	for _, class := range []string{"upstream", "manifest_invalid", "storage", "referrer_index", "lease", "other"} {
		s.metrics.taskErrors.WithLabelValues(class).Set(float64(t.errorCounts[class]))
	}
	s.metrics.taskRunnable.Set(t.runnable)
	s.metrics.taskWaiting.Set(t.waiting)
	s.metrics.recoveryUnknown.Set(t.recoveryUnknown)
}

func (s *Service) runTaskMetrics(ctx context.Context) {
	cursor, totals := inventory.Cursor{}, newTaskMetricTotals()
	s.runMetricSweep(ctx, "tasks", &cursor, func(ctx context.Context) (bool, error) {
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
	}, func() { s.publishTaskMetricTotals(totals) }, func() { cursor = inventory.Cursor{}; totals = newTaskMetricTotals() })
}
