package inventory

import (
	"context"
	"encoding/hex"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (r *Repository) Snapshot(ctx context.Context, id string, stale time.Duration) (Snapshot, error) {
	key := Prefix + "current.json"
	if id != "" {
		if !ValidID(id) {
			return Snapshot{}, ErrInvalid
		}
		key = Prefix + "snapshots/" + id + "/manifest.json"
	}
	value, _, err := Read[Snapshot](ctx, r.Store, key)
	if errors.Is(err, storage.ErrNotFound) {
		if id != "" {
			return value, ErrExpired
		}
		return value, storage.ErrUnavailable
	}
	if err != nil {
		return value, err
	}
	if value.SchemaVersion != 1 || !ValidID(value.SnapshotID) || value.ObservedAt.IsZero() {
		return value, storage.ErrCorruptObject
	}
	if !value.ExpiresAt.After(time.Now()) {
		if id == "" {
			return value, ErrCurrentExpired
		}
		return value, ErrExpired
	}
	value.Freshness = Freshness{State: "fresh", AgeSeconds: max(0, int64(time.Since(value.ObservedAt).Seconds()))}
	if time.Since(value.ObservedAt) > stale {
		value.Freshness.State = "stale"
	}
	return value, nil
}

func ValidID(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
