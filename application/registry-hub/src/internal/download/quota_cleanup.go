package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"registry-hub/internal/storage"
)

func (m *Manager) release(ctx context.Context, identity, digest, session string) error {
	key := stateKey(identity, digest)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := m.readQuota(ctx)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		reservation, exists := current.Reservations[key]
		if !exists || reservation.Session != session {
			return nil
		}
		delete(current.Reservations, key)
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if _, err := m.store.Put(ctx, quotaKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); errors.Is(err, storage.ErrConditionFailed) {
			if attempt == 15 {
				return persistenceError("quota_release", storage.ErrConditionFailed)
			}
			if waitErr := m.waitQuota(ctx, attempt); waitErr != nil {
				return persistenceError("quota_release", waitErr)
			}
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, _, readErr := m.readQuota(ctx)
			if readErr != nil {
				return persistenceError("quota_release_confirm", errors.Join(err, readErr))
			}
			committed, exists := latest.Reservations[key]
			if !exists || committed.Session != session {
				return nil
			}
			return persistenceError("quota_release_confirm", storage.ErrUnknownCommit)
		} else if err != nil {
			return persistenceError("quota_release", err)
		}
		return nil
	}
	return persistenceError("quota_release", storage.ErrConditionFailed)
}

func (m *Manager) cleanupOrphanReservations(ctx context.Context) error {
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := m.readQuota(ctx)
		if errors.Is(err, storage.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		changed := false
		removed := make([]string, 0)
		for key := range current.Reservations {
			if _, err := m.store.Head(ctx, key); errors.Is(err, storage.ErrNotFound) {
				delete(current.Reservations, key)
				removed = append(removed, key)
				changed = true
			} else if err != nil {
				return persistenceError("quota_cleanup_head", err)
			}
		}
		if !changed {
			return nil
		}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		if _, err := m.store.Put(ctx, quotaKey, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: version}); errors.Is(err, storage.ErrConditionFailed) {
			if attempt == 15 {
				return persistenceError("quota_cleanup", storage.ErrConditionFailed)
			}
			if waitErr := m.waitQuota(ctx, attempt); waitErr != nil {
				return persistenceError("quota_cleanup", waitErr)
			}
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, _, readErr := m.readQuota(ctx)
			if readErr != nil {
				return persistenceError("quota_cleanup_confirm", errors.Join(err, readErr))
			}
			for _, key := range removed {
				if _, exists := latest.Reservations[key]; exists {
					return persistenceError("quota_cleanup_confirm", storage.ErrUnknownCommit)
				}
			}
			return nil
		} else if err != nil {
			return persistenceError("quota_cleanup", err)
		}
		return nil
	}
	return persistenceError("quota_cleanup", storage.ErrConditionFailed)
}
