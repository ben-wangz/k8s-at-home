package diagnostics

import (
	"context"
	"errors"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (log BatchLog) reserve(ctx context.Context, id string, reservation EventBatchReservation) error {
	policy := log.Policy
	if policy.DailyBytes == 0 {
		policy = ObservationBatchPolicy()
	}
	rejected := false
	err := inventory.Update(ctx, log.Store, BatchBudgetKey, func(budget *EventBatchBudget) error {
		if budget.Reservations == nil {
			budget.Reservations = map[string]EventBatchReservation{}
		}
		if prior, ok := budget.Reservations[id]; ok {
			if prior != reservation {
				return storage.ErrCorruptObject
			}
			return nil
		}
		cutoff := time.Now().UTC().Add(-log.retention()).Format("20060102")
		for batchID, current := range budget.Reservations {
			if current.Date < cutoff {
				delete(budget.Reservations, batchID)
			}
		}
		var dayBytes, dayCount, globalBytes, globalCount int64
		var dayCriticalBytes, dayCriticalCount, globalCriticalBytes, globalCriticalCount int64
		for _, current := range budget.Reservations {
			globalBytes += current.Bytes
			globalCount++
			if current.Critical {
				globalCriticalBytes += current.Bytes
				globalCriticalCount++
			}
			if current.Date == reservation.Date {
				dayBytes += current.Bytes
				dayCount++
				if current.Critical {
					dayCriticalBytes += current.Bytes
					dayCriticalCount++
				}
			}
		}
		ordinaryDailyBytes := policy.DailyBytes - policy.DailyCriticalBytes
		ordinaryDailyCount := policy.DailyBatches - policy.DailyCriticalBatches
		ordinaryGlobalBytes := policy.GlobalBytes - policy.GlobalCriticalBytes
		ordinaryGlobalCount := policy.GlobalBatches - policy.GlobalCriticalBatches
		if reservation.Critical {
			rejected = dayBytes+reservation.Bytes > policy.DailyBytes || dayCount+1 > policy.DailyBatches ||
				globalBytes+reservation.Bytes > policy.GlobalBytes || globalCount+1 > policy.GlobalBatches
		} else {
			rejected = dayBytes+reservation.Bytes > ordinaryDailyBytes+dayCriticalBytes || dayCount+1 > ordinaryDailyCount+dayCriticalCount ||
				globalBytes+reservation.Bytes > ordinaryGlobalBytes+globalCriticalBytes || globalCount+1 > ordinaryGlobalCount+globalCriticalCount
		}
		if rejected {
			budget.RejectedBatches++
			if reservation.Critical {
				budget.RejectedCritical++
			}
			return nil
		}
		budget.Reservations[id] = reservation
		return nil
	})
	if err != nil {
		return err
	}
	if rejected {
		return ErrHistoryCapacity
	}
	return nil
}

func (log BatchLog) Summary(ctx context.Context, date string) (EventBatchBudgetSummary, error) {
	budget, _, err := inventory.Read[EventBatchBudget](ctx, log.Store, BatchBudgetKey)
	if errors.Is(err, storage.ErrNotFound) {
		budget, err = EventBatchBudget{}, nil
	}
	if err != nil {
		return EventBatchBudgetSummary{}, err
	}
	policy := log.Policy
	if policy.DailyBytes == 0 {
		policy = ObservationBatchPolicy()
	}
	summary := EventBatchBudgetSummary{
		Date: date, RejectedBatches: budget.RejectedBatches, RejectedCritical: budget.RejectedCritical,
		DailyBytesLimit: policy.DailyBytes, DailyBatchesLimit: policy.DailyBatches,
		GlobalBytesLimit: policy.GlobalBytes, GlobalBatchesLimit: policy.GlobalBatches,
		DailyCriticalBytes: policy.DailyCriticalBytes, DailyCriticalBatches: policy.DailyCriticalBatches,
		GlobalCriticalBytes: policy.GlobalCriticalBytes, GlobalCriticalBatches: policy.GlobalCriticalBatches,
	}
	cutoff := time.Now().UTC().Add(-log.retention()).Format("20060102")
	for _, reservation := range budget.Reservations {
		if reservation.Date < cutoff {
			continue
		}
		summary.GlobalBytes += reservation.Bytes
		summary.GlobalBatches++
		if reservation.Date == date {
			summary.DailyBytes += reservation.Bytes
			summary.DailyBatches++
		}
	}
	return summary, nil
}
