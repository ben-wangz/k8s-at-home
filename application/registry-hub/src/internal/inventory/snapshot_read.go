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
		current, _, currentErr := Read[Snapshot](ctx, r.Store, Prefix+"current.json")
		if currentErr == nil && current.SnapshotID == id {
			value := current
			return r.validateSnapshot(value, stale, true)
		}
		key = Prefix + "snapshots/" + id + "/manifest.json"
	}
	value, _, err := Read[Snapshot](ctx, r.Store, key)
	if errors.Is(err, storage.ErrNotFound) {
		if id != "" {
			return value, ErrExpired
		}
		return value, ErrNotObserved
	}
	if err != nil {
		return value, err
	}
	return r.validateSnapshot(value, stale, id != "")
}

func (r *Repository) validateSnapshot(value Snapshot, stale time.Duration, explicit bool) (Snapshot, error) {
	if value.SchemaVersion != 1 || !ValidID(value.SnapshotID) || value.ObservedAt.IsZero() {
		return value, storage.ErrCorruptObject
	}
	if !value.ExpiresAt.After(time.Now()) {
		if explicit {
			return value, ErrExpired
		}
		return value, ErrCurrentExpired
	}
	freshnessAt := value.LastSourceCheckAt
	if freshnessAt.IsZero() {
		freshnessAt = value.ObservedAt
	}
	value.Freshness = Freshness{State: "fresh", AgeSeconds: max(0, int64(time.Since(freshnessAt).Seconds()))}
	if time.Since(freshnessAt) > stale {
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
