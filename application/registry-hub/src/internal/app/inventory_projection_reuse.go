package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

type inventoryProjectionReceipt struct {
	SourceRevision string    `json:"sourceRevision"`
	Epoch          uint64    `json:"epoch"`
	ProjectedAt    time.Time `json:"projectedAt"`
}

func projectionReuseKey(id string) string { return inventory.Prefix + "task-projected/" + id + ".json" }

func (s *Service) reuseInventoryProjection(ctx context.Context, p *inventoryGraphProjection) (bool, error) {
	if p.Task.Status != tasks.Solidified || p.Task.CleanupStarted || s.gate == nil {
		return false, nil
	}
	gate := s.gate.State()
	if gate.Exclusive || gate.RecoveryBarrier || gate.State != "open" {
		return false, nil
	}
	receipt, _, err := inventory.Read[inventoryProjectionReceipt](ctx, s.store, projectionReuseKey(p.Task.ID))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil && receipt.SourceRevision == p.Version && receipt.Epoch == gate.Epoch, err
}

func (s *Service) saveInventoryProjectionReceipt(ctx context.Context, p *inventoryGraphProjection) error {
	if p.Task.Status != tasks.Solidified || p.Task.CleanupStarted {
		return nil
	}
	return inventory.Update(ctx, s.store, projectionReuseKey(p.Task.ID), func(value *inventoryProjectionReceipt) error {
		*value = inventoryProjectionReceipt{SourceRevision: p.Version, Epoch: p.Epoch, ProjectedAt: time.Now().UTC()}
		return nil
	})
}
