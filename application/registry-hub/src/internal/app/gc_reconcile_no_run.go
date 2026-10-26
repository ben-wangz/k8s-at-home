package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

type gcReconciliationConflict struct {
	reason string
}

func (e *gcReconciliationConflict) Error() string { return e.reason }

func (e *gcReconciliationConflict) Is(target error) bool {
	return target == errGCReconciliationConflict
}

var errGCReconciliationConflict = errors.New("GC reconciliation is blocked")

func (c *gcController) ReconcileNoRun(ctx context.Context, confirmedNoExecutor bool) (gcRun, error) {
	if !confirmedNoExecutor {
		return gcRun{}, &gcReconciliationConflict{reason: "explicit confirmation that no GC executor is active is required"}
	}
	if c.service.cfg.GCMode != "disabled" {
		return gcRun{}, &gcReconciliationConflict{reason: "no-run reconciliation requires GC execution to be disabled"}
	}
	if err := storage.Probe(ctx, c.service.store); err != nil {
		return gcRun{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil || c.run.State == "running" {
		return gcRun{}, &gcReconciliationConflict{reason: "a GC executor may still be active"}
	}
	if c.run.ID != "" {
		return gcRun{}, &gcReconciliationConflict{reason: "an in-memory GC run record exists"}
	}
	if err := requireMissingGCRunRecord(ctx, c.service.store); err != nil {
		return gcRun{}, err
	}
	if err := c.service.gate.ReconcileFailedUnknownNoRun(ctx); err != nil {
		return gcRun{}, err
	}
	c.run = gcRun{}
	if c.service.metrics != nil {
		c.service.metrics.setGateState(c.service.gate.State().State)
	}
	return c.run, nil
}

func requireMissingGCRunRecord(ctx context.Context, store storage.Store) error {
	object, err := store.Get(ctx, "system/gc/last-run.json")
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if object.Body == nil {
		return errors.New("GC run record returned an invalid storage body")
	}
	if closeErr := object.Body.Close(); closeErr != nil {
		return closeErr
	}
	return &gcReconciliationConflict{reason: "a durable GC run record exists; use the normal recovery operation"}
}
