package download

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type CleanupLease func(context.Context, string, string) (context.Context, func(), bool, error)

func (m *Manager) CleanupExpiredStep(ctx context.Context, entry storage.Entry, now time.Time, olderThan time.Duration, acquire CleanupLease) error {
	if !strings.HasSuffix(entry.Key, "/state.json") {
		return nil
	}
	data, object, err := m.readState(ctx, entry.Key)
	if err != nil {
		return err
	}
	var value state
	if json.Unmarshal(data, &value) != nil || value.SchemaVersion != stateSchemaVersion || value.Identity == "" || value.Digest == "" {
		return storage.ErrCorruptObject
	}
	if olderThan <= 0 || now.Sub(value.ProgressAt) < olderThan {
		return nil
	}
	leaseCtx, release, ok, err := acquire(ctx, value.Identity, value.Digest)
	if err != nil || !ok {
		return err
	}
	defer release()
	_, current, err := m.readState(leaseCtx, entry.Key)
	if err != nil {
		return err
	}
	if current.Version != object.Version {
		return nil
	}
	session := value.Session
	if len(value.GarbageSessions) > 0 {
		session = value.GarbageSessions[0]
	}
	{
		entries, _, err := m.store.List(leaseCtx, partialObjectPrefix(value.Identity, value.Digest)+"sessions/"+session+"/", "", 16)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			for _, item := range entries {
				if err := validateCleanup(leaseCtx); err != nil {
					return err
				}
				err := m.store.DeleteIfMatch(leaseCtx, item.Key, item.Version)
				if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
					return err
				}
			}
			return nil
		}
	}
	if len(value.GarbageSessions) > 0 {
		value.GarbageSessions = value.GarbageSessions[1:]
		loaded := stateVersion{Value: value, Version: current.Version}
		return m.saveState(leaseCtx, &loaded, Request{Identity: value.Identity, Digest: value.Digest, OwnerFence: value.OwnerFence, MaxBytes: int64(^uint64(0) >> 1), Guard: validateCleanup})
	}
	if err := validateCleanup(leaseCtx); err != nil {
		return err
	}
	if err := m.deleteState(leaseCtx, entry.Key, current.Version); err != nil {
		return err
	}
	if err := validateCleanup(leaseCtx); err != nil {
		return err
	}
	return m.release(leaseCtx, value.Identity, value.Digest, value.Session)
}
