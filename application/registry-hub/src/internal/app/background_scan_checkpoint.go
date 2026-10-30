package app

import (
	"context"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"time"
)

type sourceSweepCheckpoint struct {
	Status           backgroundScanStatus     `json:"status"`
	Cursor           inventory.Cursor         `json:"cursor"`
	Backoff          map[string]entityBackoff `json:"backoff"`
	Round            uint64                   `json:"round"`
	RoundFailed      bool                     `json:"roundFailed"`
	ScanRetries      int                      `json:"scanRetries"`
	NextRound        time.Time                `json:"nextRound"`
	DiagnosticOutbox []diagnostics.Event      `json:"diagnosticOutbox,omitempty"`
}

func sourceSweepKey(kind string) string { return "background/v1/source-checkpoints/" + kind + ".json" }

func (s *Service) saveSourceSweep(ctx context.Context, holder lease.Lease, kind string, checkpoint sourceSweepCheckpoint) error {
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
	if err := inventory.Update(ctx, s.store, sourceSweepKey(kind), func(value *sourceSweepCheckpoint) error {
		if value.Status.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = checkpoint
		return nil
	}); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, "background/v1/scans/"+kind+".json", func(value *backgroundScanStatus) error {
		if value.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = checkpoint.Status
		return nil
	})
}
