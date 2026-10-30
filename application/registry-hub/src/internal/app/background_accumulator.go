package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

type scanAccumulator struct {
	Capture func() any
	Restore func(json.RawMessage) error
}

func accumulatorFor[T any](value *T) *scanAccumulator {
	return &scanAccumulator{Capture: func() any { return *value }, Restore: func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return storage.ErrCorruptObject
		}
		var restored T
		if err := json.Unmarshal(raw, &restored); err != nil {
			return err
		}
		*value = restored
		return nil
	}}
}

type metricSweepCheckpoint struct {
	Status           backgroundScanStatus `json:"status"`
	Cursor           inventory.Cursor     `json:"cursor"`
	Accumulator      json.RawMessage      `json:"accumulator"`
	LastComplete     json.RawMessage      `json:"lastComplete,omitempty"`
	Invalid          bool                 `json:"invalid"`
	NextRound        time.Time            `json:"nextRound"`
	DiagnosticOutbox []diagnostics.Event  `json:"diagnosticOutbox,omitempty"`
}

func metricCheckpointKey(kind string) string {
	return "background/v1/statistics-checkpoints/" + kind + ".json"
}

func (s *Service) saveMetricSweep(ctx context.Context, holder lease.Lease, kind string, state metricSweepCheckpoint) error {
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
	if err := inventory.Update(ctx, s.store, metricCheckpointKey(kind), func(value *metricSweepCheckpoint) error {
		if value.Status.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = state
		return nil
	}); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, "background/v1/statistics/"+kind+".json", func(value *backgroundScanStatus) error {
		if value.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = state.Status
		return nil
	})
}
