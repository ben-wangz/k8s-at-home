package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"sort"
)

func (m *Manager) CleanupOrphanReservationStep(ctx context.Context, last *string, acquire CleanupLease) error {
	quota, version, err := m.readQuota(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		*last = ""
		return nil
	}
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(quota.Reservations))
	for key := range quota.Reservations {
		if key > *last {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		*last = ""
		return nil
	}
	sort.Strings(keys)
	key := keys[0]
	reservation := quota.Reservations[key]
	// Legacy reservations use the original quota version as their fencing boundary.
	if reservation.Identity != "" && reservation.Digest != "" {
		leaseCtx, release, ok, err := acquire(ctx, reservation.Identity, reservation.Digest)
		if err != nil {
			return err
		}
		if !ok {
			*last = key
			return nil
		}
		defer release()
		ctx = leaseCtx
	}
	if _, err := m.store.Head(ctx, key); err == nil {
		*last = key
		return nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	delete(quota.Reservations, key)
	data, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	if err := validateCleanup(ctx); err != nil {
		return err
	}
	_, err = m.store.Put(ctx, quotaKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version})
	if errors.Is(err, storage.ErrUnknownCommit) {
		current, _, readErr := m.readQuota(ctx)
		if readErr == nil && current.Reservations[key].Session != reservation.Session {
			err = nil
		}
	}
	if err == nil || errors.Is(err, storage.ErrConditionFailed) {
		*last = key
	}
	return err
}
