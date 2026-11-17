package app

import (
	"context"
	"errors"
	"math"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
	"time"
)

func (r *protectionRecovery) step(ctx context.Context) error {
	c := &r.checkpoint
	s := r.service
	s.protectionRecoveryStage(c.Phase, false)
	switch c.Phase {
	case "quiesce":
		if err := s.gate.WaitRecoveryQuiesced(ctx, 30*time.Second); err != nil {
			return err
		}
		r.beginValidation(false)
	case "validate_initial", "validate_final":
		return r.validateSessionStep(ctx)
	case "remote_operations":
		return r.remoteStep(ctx)
	case "representation_indexes":
		if s.cache != nil {
			if err := s.cache.RebuildRepresentationStep(ctx, &c.Representations); err != nil {
				return err
			}
			if !c.Representations.Done {
				return nil
			}
		}
		r.next("root_demand_indexes")
	case "root_demand_indexes":
		return r.rootDemandStep(ctx)
	case "cache_protection_index":
		return r.cacheReferenceStep(ctx)
	case "legacy_owners", "legacy_tasks", "legacy_admit", "legacy_link":
		return r.legacyStep(ctx)
	case "task_reconciliation", "task_reconciliation_blobs":
		return r.taskStep(ctx)
	case "release_legacy":
		processor := &solidifier{service: s, tasks: s.tasks, gate: s.gate}
		if err := processor.releaseLegacyGrandfatheredSessionIfUnowned(ctx); err != nil {
			return err
		}
		r.next("cache_reservations")
		c.CacheReservations = map[string]int64{}
	case "cache_reservations":
		return r.cacheReservationStep(ctx)
	case "finalize":
		if err := s.gate.completeProtectionRecovery(ctx, c.Generation, c.Fence, c.ValidationLedger); err != nil {
			return err
		}
		c.Phase, c.Reason, c.NextAttemptAt = "complete", "", time.Time{}
		if c.LastError != nil {
			c.RecoveredAt = time.Now().UTC()
		}
	case "complete":
	default:
		return storage.ErrCorruptObject
	}
	return nil
}

func (r *protectionRecovery) cacheReservationStep(ctx context.Context) error {
	c := &r.checkpoint
	state := r.service.gate.recoverySnapshot()
	ids := make([]string, 0, len(state.ProtectedCacheReservations))
	for id := range state.ProtectedCacheReservations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if c.Index < len(ids) {
		id := ids[c.Index]
		task, err := r.service.tasks.Get(ctx, id)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		reserved := state.ProtectedCacheReservations[id]
		active := err == nil && task.Kind == tasks.BlobKind && (task.Status == tasks.Pending || task.Status == tasks.Running || task.Status == tasks.RetryWait) && task.CacheVersion != "" && task.BlobSize >= 0 && reserved == task.BlobSize
		bytes := int64(0)
		if active {
			c.CacheReservations[id], bytes = reserved, reserved
		}
		if err == nil && task.ProtectedCacheBytes != bytes {
			if err := r.service.tasks.SetProtectedCacheBytes(ctx, id, bytes); err != nil {
				return err
			}
		}
		c.Index++
		return nil
	}
	if err := r.service.gate.ReconcileProtectedCacheReservations(ctx, c.CacheReservations); err != nil {
		return err
	}
	r.beginValidation(true)
	return nil
}

func (r *protectionRecovery) beginValidation(final bool) {
	phase := "validate_initial"
	if final {
		phase = "validate_final"
	}
	r.next(phase)
	state := r.service.gate.recoverySnapshot()
	r.checkpoint.SessionIDs = append([]string(nil), state.SessionIDs...)
	r.checkpoint.ValidationLedger = recoveryLedger(state)
	r.checkpoint.NewTotal, r.checkpoint.LegacyTotal, r.checkpoint.NewCount = 0, 0, 0
}

func (r *protectionRecovery) validateSessionStep(ctx context.Context) error {
	c := &r.checkpoint
	state := r.service.gate.recoverySnapshot()
	if recoveryLedger(state) != c.ValidationLedger {
		return storage.ErrConditionFailed
	}
	if c.Index < len(c.SessionIDs) {
		id := c.SessionIDs[c.Index]
		if !validGateID(id) || c.Index > 0 && id <= c.SessionIDs[c.Index-1] {
			return storage.ErrCorruptObject
		}
		session, err := r.service.gate.readProtectionSession(ctx, id)
		if err != nil {
			return err
		}
		if session.State == "released" || session.ReservedBytes < 0 {
			return storage.ErrCorruptObject
		}
		if c.Phase == "validate_final" && (session.PendingOperations != 0 || session.UncertainRemoteResult || session.RemoteWrite != nil || session.RemoteWritePermit != "" || session.State != "protected" && session.State != "anchored") {
			return ErrRecoveryBarrier
		}
		total := &c.NewTotal
		if session.ReservationClass == "legacy" {
			total = &c.LegacyTotal
		} else if session.ReservationClass == "new" {
			c.NewCount++
		} else {
			return storage.ErrCorruptObject
		}
		if session.ReservedBytes > math.MaxInt64-*total {
			return storage.ErrCorruptObject
		}
		*total += session.ReservedBytes
		c.Index++
		return nil
	}
	if state.ReservedDistributionBytes != c.NewTotal || state.LegacyGrandfatheredBytes != c.LegacyTotal || state.NewProtectionSessionCount != c.NewCount || state.ProtectionSessionCount != len(c.SessionIDs) {
		return storage.ErrCorruptObject
	}
	var cacheTotal int64
	for id, reserved := range state.ProtectedCacheReservations {
		if !validGateID(id) || reserved < 0 || reserved > math.MaxInt64-cacheTotal {
			return storage.ErrCorruptObject
		}
		cacheTotal += reserved
	}
	if len(state.ProtectedCacheReservations) > 10000 || len(state.ProtectedCacheReservations) != state.ProtectedCacheReservationCount || cacheTotal != state.ProtectedCacheBytes {
		return storage.ErrCorruptObject
	}
	if c.Phase == "validate_final" {
		r.next("finalize")
	} else {
		r.next("remote_operations")
	}
	return nil
}
