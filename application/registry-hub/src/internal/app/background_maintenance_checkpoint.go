package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"time"
)

type maintenanceCheckpoint struct {
	Status           backgroundScanStatus `json:"status"`
	Cursor           json.RawMessage      `json:"cursor,omitempty"`
	Next             time.Time            `json:"next"`
	Delay            time.Duration        `json:"delay"`
	DiagnosticOutbox []diagnostics.Event  `json:"diagnosticOutbox,omitempty"`
}

func maintenanceCheckpointKey(kind string) string {
	return "background/v1/maintenance-checkpoints/" + kind + ".json"
}

func (s *Service) saveMaintenanceCheckpoint(ctx context.Context, holder lease.Lease, kind string, state maintenanceCheckpoint) error {
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
	if err := inventory.Update(ctx, s.store, maintenanceCheckpointKey(kind), func(value *maintenanceCheckpoint) error {
		if value.Status.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = state
		return nil
	}); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, "background/v1/scans/"+kind+".json", func(value *backgroundScanStatus) error {
		if value.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = state.Status
		return nil
	})
}
