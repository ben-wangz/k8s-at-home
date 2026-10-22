package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"

	"registry-hub/internal/storage"
)

const maxActiveSessions = 128

var ErrQuotaExceeded = errors.New("partial download quota is exhausted")

type quotaReservation struct {
	Session string `json:"session"`
	Bytes   int64  `json:"bytes"`
}

type quotaState struct {
	Reservations map[string]quotaReservation `json:"reservations"`
}

func (m *Manager) Usage(ctx context.Context) (int64, int, error) {
	current, _, err := m.readQuota(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var reserved int64
	for _, item := range current.Reservations {
		if item.Bytes < 0 || item.Bytes > math.MaxInt64-reserved {
			return 0, 0, storage.ErrCorruptObject
		}
		reserved += item.Bytes
	}
	return reserved, len(current.Reservations), nil
}

func (m *Manager) reserve(ctx context.Context, request Request, session string, required int64) error {
	if required < 0 || required > m.maxReserved {
		return ErrQuotaExceeded
	}
	key := stateKey(request.Identity, request.Digest)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := m.readQuota(ctx)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if current.Reservations == nil {
			current.Reservations = make(map[string]quotaReservation)
		}
		old, exists := current.Reservations[key]
		used := int64(0)
		for reservationKey, reservation := range current.Reservations {
			if reservationKey != key {
				if reservation.Bytes > math.MaxInt64-used {
					return ErrQuotaExceeded
				}
				used += reservation.Bytes
			}
		}
		if !exists && len(current.Reservations) >= maxActiveSessions || required > m.maxReserved-used {
			return ErrQuotaExceeded
		}
		if exists && required < old.Bytes {
			required = old.Bytes
		}
		if request.Guard != nil {
			if err := request.Guard(ctx); err != nil {
				return err
			}
		}
		current.Reservations[key] = quotaReservation{Session: session, Bytes: required}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: version == ""}
		if version != "" {
			options.IfMatch = version
			options.IfAbsent = false
		}
		if _, err := m.store.Put(ctx, quotaKey, bytes.NewReader(data), int64(len(data)), options); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, _, readErr := m.readQuota(ctx)
			if readErr == nil {
				committed := latest.Reservations[key]
				if committed.Session == session && committed.Bytes >= required {
					return nil
				}
				continue
			}
			return errors.Join(err, readErr)
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrConditionFailed
}

func (m *Manager) readQuota(ctx context.Context) (quotaState, string, error) {
	object, err := m.store.Get(ctx, quotaKey)
	if err != nil {
		return quotaState{}, "", err
	}
	defer object.Body.Close()
	var result quotaState
	if err := json.NewDecoder(io.LimitReader(object.Body, 4<<20)).Decode(&result); err != nil || result.Reservations == nil {
		return quotaState{}, "", storage.ErrCorruptObject
	}
	return result, object.Version, nil
}

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
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, _, readErr := m.readQuota(ctx)
			if readErr == nil {
				committed, exists := latest.Reservations[key]
				if !exists || committed.Session != session {
					return nil
				}
				continue
			}
			return errors.Join(err, readErr)
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrConditionFailed
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
		for key := range current.Reservations {
			if _, err := m.store.Head(ctx, key); errors.Is(err, storage.ErrNotFound) {
				delete(current.Reservations, key)
				changed = true
			} else if err != nil {
				return err
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
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else if err != nil {
			return err
		}
		return nil
	}
	return storage.ErrConditionFailed
}

func initialReservation(size, chunkSize, maxBytes int64) (int64, error) {
	base := size
	if base < 0 {
		base = chunkSize
	}
	if base > math.MaxInt64/2 {
		return 0, ErrQuotaExceeded
	}
	result := 2 * base
	if result > maxBytes {
		return 0, ErrQuotaExceeded
	}
	return result, nil
}

func reservationFor(offset int64, maxBytes int64) int64 {
	if offset > math.MaxInt64/2 {
		return maxBytes
	}
	reserved := offset * 2
	if reserved > maxBytes {
		return maxBytes
	}
	return reserved
}

const quotaKey = "downloads/v1/quota.json"
